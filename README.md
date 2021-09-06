# Prometheus ClickHouse

This is an implementation of Prometheus's [storage adapter](https://prometheus.io/docs/prometheus/latest/storage/#remote-storage-integrations) for ClickHouse.

## Building

```shell
go build
```

## Running

```sql
-- note: replace {shard} and {replica} and run on each server

CREATE DATABASE prometheus ON CLUSTER '{cluster}';

DROP TABLE IF EXISTS prometheus.metrics;
CREATE TABLE IF NOT EXISTS prometheus.metrics ON CLUSTER '{cluster}'
(
     date Date DEFAULT toDate(0),
     name String,
     tags Array(String),
     val Float64,
     ts DateTime,
     updated DateTime DEFAULT now()
)
ENGINE = ReplicatedGraphiteMergeTree(
     '/clickhouse/tables/{shard}/prometheus.metrics',
     '{replica}', date, (name, tags, ts), 8192, 'graphite_rollup'
);
```

xml example  for clickhouse server config
```xml
 <graphite_rollup>
        <path_column_name>tags</path_column_name>
        <time_column_name>ts</time_column_name>
        <value_column_name>val</value_column_name>
        <version_column_name>updated</version_column_name>
        <default>
                <function>avg</function>
                <retention>
                        <age>0</age>
                        <precision>10</precision>
                </retention>
                <retention>
                        <age>86400</age>
                        <precision>30</precision>
                </retention>
                <retention>
                        <age>172800</age>
                        <precision>300</precision>
                </retention>
        </default>
  </graphite_rollup>
```

## Configuring Prometheus

To configure Prometheus to send samples to this binary, add the following to your `prometheus.yml`:

```yaml
# Remote write configuration (for Graphite, OpenTSDB, or InfluxDB).
remote_write:
  - url: "http://localhost:9201/write"

# Remote read configuration (for InfluxDB only at the moment).
remote_read:
  - url: "http://localhost:9201/read"
```
