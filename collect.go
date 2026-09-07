package main

import (
	"bufio"
	"bytes"
	"container/heap"
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
	"path/filepath"
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

// ---------------------------------------------------------------------------
// On-disk spill and merge.
//
// A month of spot prices is tens of millions of rows, which does not fit in
// the memory of a hosted CI runner if held as Go structs. Each (day, region)
// request is therefore sorted and written to its own zstd-compressed chunk
// file as soon as it finishes; chunks are then k-way merged per day, and the
// day files are k-way merged into the final output. Memory use is bounded by
// a single request's rows plus a handful of read buffers.
// ---------------------------------------------------------------------------

var chunkEncoderPool = sync.Pool{New: func() interface{} {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true),
		zstd.WithWindowSize(1<<20))
	if err != nil {
		panic(err)
	}
	return enc
}}

// writeSortedChunk sorts prices and writes them to path as a compressed,
// tab-separated file. Rows are ordered by SpotPrice.Compare so that files
// can later be merged without re-sorting.
func writeSortedChunk(path string, prices []SpotPrice) error {
	slices.SortFunc(prices, func(i, j SpotPrice) int { return i.Compare(j) })
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := chunkEncoderPool.Get().(*zstd.Encoder)
	defer chunkEncoderPool.Put(enc)
	enc.Reset(f)
	w := bufio.NewWriterSize(enc, 256*1024)
	for _, p := range prices {
		if err := writeChunkRow(w, p); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return f.Close()
}

func writeChunkRow(w *bufio.Writer, p SpotPrice) error {
	var buf [24]byte
	if _, err := w.Write(strconv.AppendInt(buf[:0], p.Time.UnixNano(), 10)); err != nil {
		return err
	}
	for _, field := range []string{p.ZoneID, p.Type, p.OS, p.Price} {
		if err := w.WriteByte('\t'); err != nil {
			return err
		}
		if _, err := w.WriteString(field); err != nil {
			return err
		}
	}
	return w.WriteByte('\n')
}

func parseChunkRow(line string) (SpotPrice, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 5 {
		return SpotPrice{}, fmt.Errorf("malformed chunk row %q", line)
	}
	ns, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return SpotPrice{}, fmt.Errorf("malformed chunk timestamp %q: %w", fields[0], err)
	}
	return SpotPrice{
		Time:   time.Unix(0, ns).UTC(),
		ZoneID: fields[1],
		Type:   fields[2],
		OS:     fields[3],
		Price:  fields[4],
	}, nil
}

// chunkReader streams one sorted chunk file.
type chunkReader struct {
	file    *os.File
	dec     *zstd.Decoder
	scanner *bufio.Scanner
	cur     SpotPrice
}

func openChunk(path string) (*chunkReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		f.Close()
		return nil, err
	}
	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	return &chunkReader{file: f, dec: dec, scanner: sc}, nil
}

// next advances to the following row; it returns false at end of file.
func (c *chunkReader) next() (bool, error) {
	if !c.scanner.Scan() {
		return false, c.scanner.Err()
	}
	p, err := parseChunkRow(c.scanner.Text())
	if err != nil {
		return false, err
	}
	c.cur = p
	return true, nil
}

func (c *chunkReader) close() {
	c.dec.Close()
	c.file.Close()
}

type chunkHeap []*chunkReader

func (h chunkHeap) Len() int            { return len(h) }
func (h chunkHeap) Less(i, j int) bool  { return h[i].cur.Compare(h[j].cur) < 0 }
func (h chunkHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *chunkHeap) Push(x interface{}) { *h = append(*h, x.(*chunkReader)) }
func (h *chunkHeap) Pop() interface{} {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// mergeChunks streams the rows of the given sorted chunk files in global
// sorted order, dropping exact duplicates, and calls emit for each row.
func mergeChunks(paths []string, emit func(SpotPrice) error) (int, error) {
	h := &chunkHeap{}
	for _, path := range paths {
		r, err := openChunk(path)
		if err != nil {
			return 0, err
		}
		ok, err := r.next()
		if err != nil {
			r.close()
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		if !ok {
			r.close()
			continue
		}
		heap.Push(h, r)
	}
	defer func() {
		for _, r := range *h {
			r.close()
		}
	}()
	rows := 0
	var last SpotPrice
	haveLast := false
	for h.Len() > 0 {
		r := (*h)[0]
		if !haveLast || r.cur.Compare(last) != 0 {
			if err := emit(r.cur); err != nil {
				return rows, err
			}
			last = r.cur
			haveLast = true
			rows++
		}
		ok, err := r.next()
		if err != nil {
			return rows, err
		}
		if ok {
			heap.Fix(h, 0)
		} else {
			heap.Pop(h)
			r.close()
		}
	}
	return rows, nil
}

// mergeChunksToFile merges chunk files into a new compressed chunk file.
func mergeChunksToFile(paths []string, out string) (int, error) {
	f, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	enc := chunkEncoderPool.Get().(*zstd.Encoder)
	defer chunkEncoderPool.Put(enc)
	enc.Reset(f)
	w := bufio.NewWriterSize(enc, 256*1024)
	rows, err := mergeChunks(paths, func(p SpotPrice) error { return writeChunkRow(w, p) })
	if err != nil {
		return rows, err
	}
	if err := w.Flush(); err != nil {
		return rows, err
	}
	if err := enc.Close(); err != nil {
		return rows, err
	}
	return rows, f.Close()
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
		monthEnd := AddMonths(month, 1)
		tmpDir, err := os.MkdirTemp("", "spot-"+month.Format("2006-01")+"-")
		if err != nil {
			log.Fatal(err)
		}
		type chunk struct {
			day  int
			path string
		}
		chunks := make(chan chunk, len(regions)*32)
		wg := sync.WaitGroup{}
		day := 0
		for start := month; start.Before(monthEnd); start = start.Add(24 * time.Hour) {
			for _, region := range regions {
				wg.Add(1)
				go func(day int, start time.Time, region *regionClient) {
					defer wg.Done()
					prices, err := getSpotPrices(ctx, region, start, start.Add(24*time.Hour))
					if err != nil {
						log.Fatal(err, " ", region.name)
						return
					}
					// Normalise rows to the month before spilling: drop rows
					// past the end of the month and clamp the pre-month price
					// that AWS returns to the start of the month.
					kept := prices[:0]
					for _, p := range prices {
						if !p.Time.Before(monthEnd) {
							continue
						}
						if p.Time.Before(month) {
							p.Time = month
						}
						kept = append(kept, p)
					}
					path := filepath.Join(tmpDir, fmt.Sprintf("d%02d-%s.zst", day, region.name))
					if err := writeSortedChunk(path, kept); err != nil {
						log.Fatal("Failed to write chunk ", path, ": ", err)
						return
					}
					chunks <- chunk{day, path}
				}(day, start, region)
			}
			day++
		}
		numDays := day

		go func() {
			wg.Wait()
			close(chunks)
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

		chunksByDay := make([][]string, numDays)
		for c := range chunks {
			chunksByDay[c.day] = append(chunksByDay[c.day], c.path)
		}
		cancelMonitor()

		// First level: merge each day's per-region chunks into one file per
		// day, keeping the number of simultaneously open files small.
		log.Println("Merging daily chunks")
		dayFiles := make([]string, 0, numDays)
		for d, paths := range chunksByDay {
			out := filepath.Join(tmpDir, fmt.Sprintf("day%02d.zst", d))
			if _, err := mergeChunksToFile(paths, out); err != nil {
				log.Fatal("Failed to merge day ", d, ": ", err)
			}
			for _, p := range paths {
				os.Remove(p)
			}
			dayFiles = append(dayFiles, out)
		}

		// Second level: merge the day files straight into the final output.
		var out bytes.Buffer
		log.Println("Merging and compressing")
		zstw, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if err != nil {
			log.Fatal(err)
		}
		enc := csv.NewWriter(zstw)
		enc.Comma = '\t'
		rows, err := mergeChunks(dayFiles, func(price SpotPrice) error {
			return enc.Write([]string{price.ZoneID, price.Type, price.OS, price.Price, price.Time.Format(time.RFC3339)})
		})
		if err != nil {
			log.Fatal("Failed to merge month: ", err)
		}
		enc.Flush()
		if err := enc.Error(); err != nil {
			log.Fatal(err)
		}
		if err := zstw.Close(); err != nil {
			log.Fatal(err)
		}
		os.RemoveAll(tmpDir)
		collectedPrices[mapKey] = out.Bytes()
		log.Println("Writing to", mapKey)
		if err := os.WriteFile(mapKey, out.Bytes(), 0644); err != nil {
			log.Fatal(err)
		}
		log.Println("Collected", rows, "prices")
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
