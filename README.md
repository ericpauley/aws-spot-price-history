# AWS Spot Price History

*Update:* This dataset is now hosted on Zenodo: [![DOI](https://zenodo.org/badge/DOI/10.5281/zenodo.14198917.svg)](https://doi.org/10.5281/zenodo.14198917)

This repository tracks historical prices for AWS spot prices across all regions. It is updated automatically on the 1st of each month to contain data from the previous month.

# Data format

Each month of data is stored as a ZStandard-compressed `.tsv.zst` file in the `prices` folder.

The data format matches that returned by AWS's `describe-spot-instance-prices`, with the exception that availability zones have been replaced by their global ID. For instance, here are some example lines from one capture:

```
euc1-az2        i4i.8xlarge     Linux/UNIX      1.231800        2023-02-28T23:59:57+00:00
euc1-az3        r5b.8xlarge     Red Hat Enterprise Linux        0.749600        2023-02-28T23:59:58+00:00
euc1-az3        r5b.8xlarge     SUSE Linux      0.744600        2023-02-28T23:59:58+00:00
euc1-az3        r5b.8xlarge     Linux/UNIX      0.619600        2023-02-28T23:59:58+00:00
euc1-az3        m5n.4xlarge     Red Hat Enterprise Linux        0.476000        2023-02-28T23:59:59+00:00
euc1-az2        m5n.4xlarge     Red Hat Enterprise Linux        0.492000        2023-02-28T23:59:59+00:00
euc1-az3        m5n.4xlarge     SUSE Linux      0.471000        2023-02-28T23:59:59+00:00
euc1-az2        m5n.4xlarge     SUSE Linux      0.487000        2023-02-28T23:59:59+00:00
euc1-az3        m5n.4xlarge     Linux/UNIX      0.346000        2023-02-28T23:59:59+00:00
euc1-az2        m5n.4xlarge     Linux/UNIX      0.362000        2023-02-28T23:59:59+00:00
```

When fetching spot instance pricing from AWS, results contain some prices from the previous month so that the price is known at the start of the month. These prices are adjusted in this dataset to be at the exact start of the month UTC:

```
euw3-az2        g4dn.4xlarge    Linux/UNIX      0.558600        2023-01-01T00:00:00+00:00
```

For data from 2023-01 and before, this data was fetched more than one month at a time. This should have no negative impact unless, for example, an instance type was retired before the month began (and there should therefore be no price). These older files also only contain default regions. Data from 2023-02 and later contains all regions, including opt-in regions.

# Using data

You can process each month individually. If you need the entire data stream at once, you can cat all files to `zst` together:

```
cat prices/*/*.tsv.zst | zstd -d
```