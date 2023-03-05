import json
import boto3
from functools import cache
import zstandard
import io
import os
import datetime
from dateutil.relativedelta import relativedelta
import multiprocessing

import time
import random


@cache
def get_zone_ids(region):
    ec2 = boto3.client("ec2", region_name=region)

    azs = ec2.describe_availability_zones()["AvailabilityZones"]
    return dict((az["ZoneName"], az["ZoneId"]) for az in azs)


@cache
def get_zone_id(zone):
    return get_zone_ids(zone[:-1])[zone]


def import_old(name):
    with open(name, "rb") as infile:
        costs = {}
        dctx = zstandard.ZstdDecompressor()
        stream_reader = dctx.stream_reader(infile)
        text_stream = io.TextIOWrapper(stream_reader)
        skipped = 0
        bucket = None
        outfile = None
        outzst = None
        for line in text_stream:
            zone, instance, os_name, cost, time = line.strip().split("\t")
            zoneid = get_zone_id(zone)
            costkey = (zoneid, instance, os_name)
            if costs.get(costkey) == cost:
                skipped += 1
                if skipped % 10000 == 0:
                    print(skipped)
                continue
            year, month, dt = time.split("-")
            if bucket != (year, month):
                bucket = (year, month)
                if outfile:
                    outzst.close()
                    outfile.close()
                os.makedirs(f"prices/{year}", exist_ok=True)
                outfile = open(f"prices/{year}/{month}.tsv.zst", "wb")
                outzst = zstandard.ZstdCompressor(level=10).stream_writer(outfile)
                new_t = f"{year}-{month}-01T00:00:00+00:00"
                for (z, i, o), c in costs.items():
                    outzst.write(
                        ("\t".join([z, i, o, c, new_t]) + "\n").encode("utf-8")
                    )
            costs[costkey] = cost

            outzst.write(
                ("\t".join([zoneid, instance, os_name, cost, time]) + "\n").encode(
                    "utf-8"
                )
            )
        if outfile:
            outzst.close()
            outfile.close()


def get_spot_prices(region, start, end):
    ec2_regional = boto3.client("ec2", region_name=region)
    NextToken = ""
    region_results = []
    while True:
        history = ec2_regional.describe_spot_price_history(
            MaxResults=1000,
            StartTime=start,
            EndTime=end,
            NextToken=NextToken,
        )
        for item in history["SpotPriceHistory"]:
            region_results.append(tuple(item.values()))
        NextToken = history["NextToken"]
        if not NextToken:
            break
    return region_results


def fetch_recent_data():
    ec2_global = boto3.client("ec2", region_name="us-east-1")
    regions = ec2_global.describe_regions()
    now = datetime.datetime.now().astimezone(datetime.timezone.utc)
    start_of_month = datetime.datetime(
        now.year, now.month, 1, tzinfo=datetime.timezone.utc
    )
    start_of_last_month = start_of_month - relativedelta(months=1)
    date = start_of_last_month
    date_buckets = []
    while date < start_of_month:
        date_buckets.append(date)
        date = date + relativedelta(days=10)
    date_buckets.append(start_of_month)
    jobs = [
        (region["RegionName"], start, end)
        for start, end in zip(date_buckets, date_buckets[1:])
        for region in regions["Regions"]
    ]
    p: multiprocessing.Pool = multiprocessing.Pool(32)
    print(f"Starting {len(jobs)} jobs... E.g., ", jobs[:3])
    rs = p.starmap_async(get_spot_prices, jobs, chunksize=1)
    p.close()  # No more work
    while True:
        if rs.ready():
            break
        remaining = rs._number_left
        print("Waiting for", remaining, "tasks to complete...")
        time.sleep(2)
    all_results = rs.get()

    results = [result for region in all_results for result in region]

    results.sort(key=lambda k: k[4])

    os.makedirs(f"prices/{start_of_last_month.year}", exist_ok=True)
    with open(
        f"prices/{start_of_last_month.year}/{start_of_last_month.month:02d}.tsv.zst",
        "wb",
    ) as outfile:
        with zstandard.ZstdCompressor(level=10).stream_writer(outfile) as outzst:
            costs = {}
            for zone, instance, os_name, cost, record_time in results:
                record_time: datetime.datetime
                if record_time < start_of_last_month:
                    record_time = start_of_last_month
                if record_time >= start_of_month:
                    continue
                zoneid = get_zone_id(zone)
                costkey = (zoneid, instance, os_name)
                if costs.get(costkey) == cost:
                    continue
                costs[costkey] = cost

                outzst.write(
                    (
                        "\t".join(
                            [zoneid, instance, os_name, cost, record_time.isoformat()]
                        )
                        + "\n"
                    ).encode("utf-8")
                )


if __name__ == "__main__":
    fetch_recent_data()
