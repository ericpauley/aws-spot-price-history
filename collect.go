package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/account"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/exp/maps"
	"golang.org/x/time/rate"
)

const zenodoURL = "https://zenodo.org"

const (
	regionProbeTimeout = 2 * time.Minute
	zenodoMaxAttempts  = 6
	zenodoRetryBase    = 10 * time.Second
)

var limiters = make(map[string]*rate.Limiter)
var limiterMutex sync.Mutex

func getLimiter(region string) *rate.Limiter {
	limiterMutex.Lock()
	defer limiterMutex.Unlock()
	if limiter, ok := limiters[region]; ok {
		return limiter
	}
	limiter := rate.NewLimiter(15, 80)
	limiters[region] = limiter
	return limiter
}

type SpotPrice struct {
	ZoneID string
	Type   string
	OS     string
	Price  string
	Time   time.Time
}

func (i SpotPrice) Compare(j SpotPrice) int {
	if i.Time.Before(j.Time) {
		return -1
	}
	if i.Time.After(j.Time) {
		return 1
	}
	if i.ZoneID < j.ZoneID {
		return -1
	}
	if i.ZoneID > j.ZoneID {
		return 1
	}
	if i.Type < j.Type {
		return -1
	}
	if i.Type > j.Type {
		return 1
	}
	if i.OS < j.OS {
		return -1
	}
	if i.OS > j.OS {
		return 1
	}
	if i.Price < j.Price {
		return -1
	}
	if i.Price > j.Price {
		return 1
	}
	return 0
}

type SpotPriceRequest struct {
	region string
	start  time.Time
	end    time.Time
}

var spotPriceRequestProgress = make(map[SpotPriceRequest]float64)
var spotPriceRequestMutex sync.Mutex

func getSpotPrices(ctx context.Context, region *regionClient, start time.Time, end time.Time) ([]SpotPrice, error) {
	spotPriceRequestMutex.Lock()
	spotPriceRequestProgress[SpotPriceRequest{region.name, start, end}] = 0
	spotPriceRequestMutex.Unlock()
	limiter := getLimiter(region.name)
	svc := region.svc
	zoneIdMapping := region.zoneIDs

	var prices []SpotPrice
	paginator := ec2.NewDescribeSpotPriceHistoryPaginator(svc, &ec2.DescribeSpotPriceHistoryInput{
		StartTime:  &start,
		EndTime:    &end,
		MaxResults: aws.Int32(1000),
	})
	for paginator.HasMorePages() {
		err := limiter.Wait(ctx)
		if err != nil {
			return nil, err
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		if len(page.SpotPriceHistory) != 0 {
			progress := end.Sub(*page.SpotPriceHistory[0].Timestamp).Seconds() / end.Sub(start).Seconds()
			spotPriceRequestMutex.Lock()
			spotPriceRequestProgress[SpotPriceRequest{region.name, start, end}] = progress
			spotPriceRequestMutex.Unlock()
		}
		for _, price := range page.SpotPriceHistory {
			prices = append(prices, SpotPrice{
				ZoneID: zoneIdMapping[*price.AvailabilityZone],
				Type:   string(price.InstanceType),
				OS:     string(price.ProductDescription),
				Price:  *price.SpotPrice,
				Time:   *price.Timestamp,
			})
			// log.Println(zoneIdMapping[*price.AvailabilityZone], price.InstanceType, price.ProductDescription, *price.SpotPrice, *price.Timestamp)
		}
	}
	spotPriceRequestMutex.Lock()
	spotPriceRequestProgress[SpotPriceRequest{region.name, start, end}] = 1
	spotPriceRequestMutex.Unlock()
	return prices, nil
}

// regionClient holds a ready-to-use EC2 client for a region along with the
// zone name -> zone ID mapping needed to normalise spot price records.
type regionClient struct {
	name    string
	svc     *ec2.Client
	zoneIDs map[string]string
}

// isUnreachable reports whether err is a network-level failure (DNS, dial,
// TLS or I/O timeout) rather than an API error. Regions that are entirely
// offline surface as these errors.
func isUnreachable(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

// probeRegion builds an EC2 client for the region and fetches its zone ID
// mapping. It is used both to detect regions that are completely unreachable
// and to avoid re-fetching the mapping for every collection request.
func probeRegion(ctx context.Context, region string) (*regionClient, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("configuration error: %w", err)
	}
	svc := ec2.NewFromConfig(cfg)
	probeCtx, cancel := context.WithTimeout(ctx, regionProbeTimeout)
	defer cancel()
	zones, err := svc.DescribeAvailabilityZones(probeCtx, &ec2.DescribeAvailabilityZonesInput{AllAvailabilityZones: aws.Bool(true)})
	if err != nil {
		return nil, err
	}
	zoneIDs := make(map[string]string)
	for _, zone := range zones.AvailabilityZones {
		zoneIDs[*zone.ZoneName] = *zone.ZoneId
	}
	return &regionClient{name: region, svc: svc, zoneIDs: zoneIDs}, nil
}

// probeRegions probes every region concurrently. Regions that cannot be
// reached at the network level (e.g. a full regional outage) are logged and
// dropped; any other error is returned.
func probeRegions(ctx context.Context, regions []string) ([]*regionClient, []string, error) {
	type result struct {
		client *regionClient
		err    error
	}
	results := make([]result, len(regions))
	var wg sync.WaitGroup
	for i, region := range regions {
		wg.Add(1)
		go func(i int, region string) {
			defer wg.Done()
			client, err := probeRegion(ctx, region)
			results[i] = result{client, err}
		}(i, region)
	}
	wg.Wait()

	var clients []*regionClient
	var skipped []string
	for i, region := range regions {
		r := results[i]
		if r.err == nil {
			clients = append(clients, r.client)
			continue
		}
		if isUnreachable(r.err) {
			log.Printf("WARNING: region %s is unreachable and will be skipped: %v", region, r.err)
			skipped = append(skipped, region)
			continue
		}
		return nil, nil, fmt.Errorf("probing region %s: %w", region, r.err)
	}
	if len(clients) == 0 {
		return nil, nil, errors.New("no regions reachable")
	}
	return clients, skipped, nil
}

func getEC2Regions(ctx context.Context) ([]string, error) {
	// Get all regions
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	svc := ec2.NewFromConfig(cfg)
	accountManagement := account.NewFromConfig(cfg)
	regions, err := svc.DescribeRegions(ctx, &ec2.DescribeRegionsInput{AllRegions: aws.Bool(true)})
	if err != nil {
		return nil, fmt.Errorf("failed to describe regions: %w", err)
	}
	var regionNames []string
	enabledRegion := false

	for _, region := range regions.Regions {
		if *region.OptInStatus == "not-opted-in" {
			log.Print("Not opted into region ", *region.RegionName)
			_, err := accountManagement.EnableRegion(ctx, &account.EnableRegionInput{RegionName: region.RegionName})
			if err != nil {
				return nil, fmt.Errorf("failed to enable region %s: %w", *region.RegionName, err)
			}
			enabledRegion = true
		}
		regionNames = append(regionNames, *region.RegionName)
	}
	if enabledRegion {
		log.Print("Enabled region, waiting 15min for it to be enabled")
		time.Sleep(15 * time.Minute)
		return getEC2Regions(ctx)
	}

	return regionNames, nil
}

type ZenodoResourceType struct {
	Title string `json:"title"`
	Type  string `json:"type"`
}

func (z ZenodoResourceType) MarshalJSON() ([]byte, error) {
	return json.Marshal(z.Title)
}

type ZenodoMeta struct {
	Title       string `json:"title"`
	UploadType  string `json:"upload_type"`
	Description string `json:"description"`
	Creators    []struct {
		Name        string `json:"name"`
		Affiliation string `json:"affiliation"`
	} `json:"creators"`
	// AccessRight string `json:"access_right"`
	Custom struct {
		CodeRepository string `json:"code:codeRepository"`
	} `json:"custom"`
	// ResourceType ZenodoResourceType `json:"resource_type"`
	License string `json:"license"`
	Version string `json:"version,omitempty"`
}

type ZenodoVersion struct {
	Created  time.Time `json:"created"`
	DOI      string    `json:"doi"`
	Modified time.Time `json:"modified"`
	Files    []struct {
		ID       string `json:"id"`
		Filename string `json:"filename"`
		Size     int    `json:"filesize"`
		Checksum string `json:"checksum"`
		Links    struct {
			Self     string `json:"self"`
			Download string `json:"download"`
		}
	} `json:"files"`
	MetaData map[string]interface{} `json:"metadata"`
	Links    map[string]string      `json:"links"`
	ID       int                    `json:"id"`
}

type ZenodoError struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func doZenodoRequest(ctx context.Context, method, path string, request interface{}, result interface{}) error {
	if strings.HasPrefix(path, "/") {
		return doZenodoRequest(ctx, method, zenodoURL+path, request, result)
	}
	var rawBody []byte
	contentType := ""
	if request != nil {
		if raw, ok := request.([]byte); ok {
			log.Println("Uploading raw body")
			rawBody = raw
			contentType = "application/octet-stream"
		} else {
			contentType = "application/json"
			var b bytes.Buffer
			if err := json.NewEncoder(&b).Encode(request); err != nil {
				return err
			}
			rawBody = b.Bytes()
		}
	}
	var lastErr error
	for attempt := 0; attempt < zenodoMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * zenodoRetryBase
			log.Printf("Zenodo request %s %s failed (%v), retrying in %s", method, path, lastErr, delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		var retry bool
		retry, lastErr = doZenodoRequestOnce(ctx, method, path, rawBody, contentType, result)
		if lastErr == nil || !retry {
			return lastErr
		}
	}
	return lastErr
}

// doZenodoRequestOnce performs a single request. The returned bool reports
// whether the error is transient and the request should be retried.
func doZenodoRequestOnce(ctx context.Context, method, url string, rawBody []byte, contentType string, result interface{}) (bool, error) {
	var body io.Reader
	if contentType != "" {
		body = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return false, err
	}
	// Send the token as a header rather than a query parameter: Zenodo now
	// redirects some endpoints (e.g. /versions/latest), and a query parameter
	// would be dropped on the redirect while the header is preserved.
	if token := os.Getenv("ZENODO_ACCESS_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aws-spot-price-history (https://github.com/ericpauley/aws-spot-price-history)")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, fmt.Errorf("reading response from %s %s: %w", method, url, err)
	}
	isJSON := strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
	transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	if resp.StatusCode >= 400 {
		var zenodoError ZenodoError
		if isJSON && json.Unmarshal(respBody, &zenodoError) == nil && zenodoError.Message != "" {
			return transient, fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, zenodoError.Message)
		}
		return transient, fmt.Errorf("%s %s: HTTP %d (%s): %s", method, url, resp.StatusCode, resp.Header.Get("Content-Type"), snippet(respBody))
	}
	if result == nil {
		return false, nil
	}
	if !isJSON {
		// An HTML page with a 2xx status is almost always a proxy/maintenance
		// page standing in for the API; treat it as transient.
		return true, fmt.Errorf("%s %s: HTTP %d returned non-JSON response (%s): %s", method, url, resp.StatusCode, resp.Header.Get("Content-Type"), snippet(respBody))
	}
	if err := json.Unmarshal(respBody, result); err != nil {
		return false, fmt.Errorf("%s %s: decoding response: %w: %s", method, url, err, snippet(respBody))
	}
	return false, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func getLatestZenodoVersion(ctx context.Context) (*ZenodoVersion, error) {
	type ZenodoVersionResponse struct {
		ID int `json:"id"`
	}
	recordID := os.Getenv("ZENODO_RECORD_ID")
	var resp ZenodoVersionResponse
	err := doZenodoRequest(ctx, "GET", "/api/records/"+recordID+"/versions/latest", nil, &resp)
	if err != nil {
		return nil, err
	}
	var version ZenodoVersion
	err = doZenodoRequest(ctx, "GET", "/api/deposit/depositions/"+strconv.Itoa(resp.ID), nil, &version)
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func newZenodoVersion(ctx context.Context, oldVersion *ZenodoVersion) (*ZenodoVersion, error) {
	versionID := oldVersion.ID
	var versionResponse ZenodoVersion
	err := doZenodoRequest(ctx, "POST", "/api/deposit/depositions/"+strconv.Itoa(versionID)+"/actions/newversion", nil, &versionResponse)
	if err != nil {
		return nil, err
	}
	draftLink := versionResponse.Links["latest_draft"]
	idParts := strings.Split(draftLink, "/")
	draftID, err := strconv.Atoi(idParts[len(idParts)-1])
	if err != nil {
		return nil, err
	}
	var newVersion ZenodoVersion
	err = doZenodoRequest(ctx, "GET", "/api/deposit/depositions/"+strconv.Itoa(draftID), nil, &newVersion)
	if err != nil {
		return nil, err
	}
	return &newVersion, nil
}

func setZenodoMeta(ctx context.Context, draftID int, meta map[string]interface{}) error {
	err := doZenodoRequest(ctx, "PUT", "/api/deposit/depositions/"+strconv.Itoa(draftID), map[string]interface{}{"metadata": meta}, nil)
	if err != nil {
		return err
	}
	return nil
}

func AddMonths(t time.Time, months int) time.Time {
	year, month, _ := t.Date()
	return time.Date(year, time.Month(int(month)+months), 1, 0, 0, 0, 0, time.UTC)
}

func GetApplicableDateRanges(ver ZenodoVersion) []time.Time {

	candidateMonths := []time.Time{AddMonths(time.Now(), -1), AddMonths(time.Now(), -2)}
	var months []time.Time
	log.Println("Files", ver.Files)
	for _, month := range candidateMonths {
		if time.Since(month) > 89*24*time.Hour {
			continue
		}
		alreadyCollected := false
		for _, file := range ver.Files {
			if file.Filename == month.Format("2006-01")+".tsv.zst" {
				alreadyCollected = true
				break
			}
			if file.Filename == month.Format("2006")+".tsv.zst" {
				alreadyCollected = true
				break
			}
		}
		if alreadyCollected && !month.Equal(AddMonths(time.Now(), -1)) {
			continue
		}
		months = append(months, month)
	}

	return months
}

func main() {
	ctx := context.Background()
	regionNames, err := getEC2Regions(ctx)
	if err != nil {
		log.Fatal(err)
	}
	regions, skippedRegions, err := probeRegions(ctx, regionNames)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Collecting from %d regions", len(regions))
	if len(skippedRegions) > 0 {
		log.Printf("WARNING: skipping unreachable regions: %s", strings.Join(skippedRegions, ", "))
	}
	version, err := getLatestZenodoVersion(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	months := GetApplicableDateRanges(*version)
	// months := []time.Time{AddMonths(time.Now(), -1)}
	log.Println(months)
	latestMonth := AddMonths(time.Now(), -1)

	collectedPrices := make(map[string][]byte)
	for _, month := range months {
		log.Println("Collecting data for", month)
		mapKey := month.Format("2006-01") + ".tsv.zst"
		if _, err := os.Stat(mapKey); err == nil {
			log.Println("Already collected", mapKey)
			collectedPrices[mapKey], err = os.ReadFile(mapKey)
			if err != nil {
				log.Fatal(err)
			}
			continue
		}
		priceResults := make(chan []SpotPrice, 1000000)
		wg := sync.WaitGroup{}
		for start := month; start.Before(AddMonths(month, 1)); start = start.Add(24 * time.Hour) {
			for _, region := range regions {
				wg.Add(1)
				go func(region *regionClient) {
					defer wg.Done()
					prices, err := getSpotPrices(ctx, region, start, start.Add(24*time.Hour))
					if err != nil {
						log.Fatal(err, " ", region.name)
						return
					}
					priceResults <- prices
				}(region)
			}
		}

		go func() {
			wg.Wait()
			close(priceResults)
		}()

		monitorContext, cancelMonitor := context.WithCancel(ctx)

		go func() {
			for range time.Tick(5 * time.Second) {
				if monitorContext.Err() != nil {
					return
				}
				// Get the minimum progress
				spotPriceRequestMutex.Lock()
				if len(spotPriceRequestProgress) != 0 {
					minProgress := slices.Min(maps.Values(spotPriceRequestProgress))
					log.Printf("Progress: %.1f%%\n", minProgress*100)
				}
				spotPriceRequestMutex.Unlock()
			}
		}()

		var allPrices []SpotPrice

		for prices := range priceResults {
			allPrices = append(allPrices, prices...)
		}
		cancelMonitor()
		log.Println("Sorting")
		slices.SortFunc(allPrices, func(i, j SpotPrice) int {
			return i.Compare(j)
		})
		var out bytes.Buffer
		log.Println("Compressing")
		zstw, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			log.Fatal(err)
		}
		enc := csv.NewWriter(zstw)
		enc.Comma = '\t'
		var lastRow SpotPrice
		for _, price := range allPrices {
			if !price.Time.Before(AddMonths(month, 1)) {
				// Don't include prices from other months
				continue
			}
			if price.Time.Before(month) {
				price.Time = month
			}
			if price.Compare(lastRow) == 0 {
				continue
			}
			lastRow = price
			row := []string{price.ZoneID, price.Type, price.OS, price.Price, price.Time.Format(time.RFC3339)}
			if err := enc.Write(row); err != nil {
				log.Fatal(err)
			}
		}
		enc.Flush()
		if err := zstw.Close(); err != nil {
			log.Fatal(err)
		}
		collectedPrices[mapKey] = out.Bytes()
		log.Println("Writing to", mapKey)
		if err := os.WriteFile(mapKey, out.Bytes(), 0644); err != nil {
			log.Fatal(err)
		}
		// priceSlice := maps.Keys(allPrices)
		// sort.Slice(priceSlice, func(i, j int) bool {
		// 	return priceSlice[i].Time.Before(priceSlice[j].Time)
		// })
		log.Println("Collected", len(allPrices), "prices")
	}
	log.Println("Uploading to Zenodo")

	newVersion, err := newZenodoVersion(ctx, version)
	if err != nil {
		log.Fatal("Failed to create version", err)
	}
	meta := newVersion.MetaData
	meta["version"] = latestMonth.Format("2006-01")
	if err := setZenodoMeta(ctx, newVersion.ID, meta); err != nil {
		log.Fatal("Failed to update meta", err)
	}

	for month, data := range collectedPrices {
		log.Println("Uploading", month)
		var response map[string]interface{}
		err := doZenodoRequest(ctx, "PUT", newVersion.Links["bucket"]+"/"+month, data, &response)
		if err != nil {
			log.Fatal("Failed to upload", err)
		}
		log.Println("Uploaded", month, response)
	}
	if len(skippedRegions) > 0 {
		log.Printf("WARNING: data for unreachable regions is missing from this upload: %s", strings.Join(skippedRegions, ", "))
	}
	if os.Getenv("ZENODO_PUBLISH") == "publish" {
		log.Println("Publishing")
		err := doZenodoRequest(ctx, "POST", "/api/deposit/depositions/"+strconv.Itoa(newVersion.ID)+"/actions/publish", nil, nil)
		if err != nil {
			log.Fatal("Failed to publish", err)
		}
	}
}
