// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The main package for the Prometheus server executable.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go"
	_ "github.com/ClickHouse/clickhouse-go"
	"github.com/EinKrebs/prometheus-clickhouse/client"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/prometheus/prometheus/prompb"
)

type config struct {
	url           string
	username      string
	password      string
	database      string
	table         string
	caPath        string
	readTimeout   time.Duration
	writeTimeout  time.Duration
	altHosts      string
	remoteTimeout time.Duration
	listenAddr    string
	telemetryPath string
	promlogConfig promlog.Config
}

var (
	receivedSamples = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "received_samples_total",
			Help: "Total number of received samples.",
		},
	)
	sentSamples = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sent_samples_total",
			Help: "Total number of processed samples sent to remote storage.",
		},
		[]string{"remote"},
	)
	failedSamples = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "failed_samples_total",
			Help: "Total number of processed samples which failed on send to remote storage.",
		},
		[]string{"remote"},
	)
	sentBatchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sent_batch_duration_seconds",
			Help:    "Duration of sample batch send calls to the remote storage.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"remote"},
	)
)

func init() {
	prometheus.MustRegister(receivedSamples)
	prometheus.MustRegister(sentSamples)
	prometheus.MustRegister(failedSamples)
	prometheus.MustRegister(sentBatchDuration)
}

func main() {
	cfg := parseFlags()
	http.Handle(cfg.telemetryPath, promhttp.Handler())

	logger := promlog.New(&cfg.promlogConfig)

	writer, reader := buildClient(logger, cfg)
	if err := serve(logger, cfg.listenAddr, writer, reader); err != nil {
		_ = level.Error(logger).Log("msg", "Failed to listen", "addr", cfg.listenAddr, "err", err)
		os.Exit(1)
	}
}

func parseFlags() *config {
	a := kingpin.New(filepath.Base(os.Args[0]), "Remote storage adapter")
	a.HelpFlag.Short('h')

	cfg := &config{
		password:      os.Getenv("CLICKHOUSE_PW"),
		promlogConfig: promlog.Config{},
	}

	a.Flag("db.url", "The URL of the remote Clickhouse server to send samples to. None, if empty.").
		Default("").StringVar(&cfg.url)
	a.Flag("db.username", "The username to use when sending samples to Clickhouse. The corresponding password must be provided via the CLICKHOUSE_PW environment variable.").
		Default("").StringVar(&cfg.username)
	a.Flag("db.database", "The name of the database to use for storing samples in Clickhouse.").
		Default("prometheus").StringVar(&cfg.database)
	a.Flag("db.table", "The name of the table to use for storing samples in Clickhouse.").
		Default("metrics").StringVar(&cfg.table)
	a.Flag("db.ca-file-path", "Path to CA-certificate used to connect with TLS.").
		Default("").StringVar(&cfg.caPath)
	a.Flag("db.read-timeout", "The timeout to use when read metrics from the Clickhouse.").
		Default("10s").DurationVar(&cfg.readTimeout)
	a.Flag("db.write-timeout", "The timeout to use when write metrics to the Clickhouse.").
		Default("10s").DurationVar(&cfg.writeTimeout)
	a.Flag("db.althosts", "The CLuster URL of the remote Clickhouse server to send samples to. None, if empty.").
		Default("").StringVar(&cfg.altHosts)
	a.Flag("send-timeout", "The timeout to use when sending samples to the remote storage.").
		Default("30s").DurationVar(&cfg.remoteTimeout)
	a.Flag("web.listen-address", "Address to listen on for web endpoints.").
		Default(":9201").StringVar(&cfg.listenAddr)
	a.Flag("web.telemetry-path", "Address to listen on for web endpoints.").
		Default("/metrics").StringVar(&cfg.telemetryPath)

	flag.AddFlags(a, &cfg.promlogConfig)

	_, err := a.Parse(os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, errors.Wrapf(err, "Error parsing commandline arguments"))
		a.Usage(os.Args[1:])
		os.Exit(2)
	}

	return cfg
}

type logWriter interface {
	Write(samples model.Samples) error
	Name() string
}

type logReader interface {
	Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error)
	Name() string
}

func buildClient(logger log.Logger, cfg *config) (logWriter, logReader) {
	options := make(url.Values)
	options.Set("database", cfg.database)
	options.Set("username", cfg.username)
	options.Set("password", cfg.password)
	options.Set("read_timeout", cfg.readTimeout.String())
	options.Set("write_timeout", cfg.writeTimeout.String())
	options.Set("alt_hosts", cfg.altHosts)
	const tlsConfigKey = "clickhouse_tls_config_key"
	if cfg.caPath != "" {
		caCert, err := ioutil.ReadFile(cfg.caPath)
		if err != nil {
			_ = level.Error(logger).Log("read ca-certificate", err)
			os.Exit(1)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		err = clickhouse.RegisterTLSConfig(tlsConfigKey, &tls.Config{RootCAs: caCertPool})
		if err != nil {
			_ = level.Error(logger).Log("register tls config", err)
			os.Exit(1)
		}
		options.Set("tls_config", tlsConfigKey)
		options.Set("secure", "true")
	}

	dsn := (&url.URL{
		Scheme:   "tcp",
		Host:     cfg.url,
		RawQuery: options.Encode(),
	}).String()

	c := client.New(
		log.With(logger, "storage", "Clickhouse"),
		dsn,
		cfg.database,
		cfg.table,
	)
	prometheus.MustRegister(c)
	_ = level.Info(logger).Log("msg", "Starting up...")
	return c, c
}

func serve(logger log.Logger, addr string, writer logWriter, reader logReader) error {
	http.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			_ = level.Error(logger).Log("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			_ = level.Error(logger).Log("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.WriteRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			_ = level.Error(logger).Log("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		samples := protoToSamples(&req)
		receivedSamples.Add(float64(len(samples)))

		var wg sync.WaitGroup
		wg.Add(1)
		go func(writer logWriter) {
			sendSamples(logger, writer, samples)
			wg.Done()
		}(writer)
		wg.Wait()
	})

	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			_ = level.Error(logger).Log("msg", "Read error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			_ = level.Error(logger).Log("msg", "Decode error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.ReadRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			_ = level.Error(logger).Log("msg", "Unmarshal error", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var resp *prompb.ReadResponse
		resp, err = reader.Read(&req)
		if err != nil {
			_ = level.Warn(logger).Log("msg", "Error executing query", "query", req, "storage", reader.Name(), "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		data, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "snappy")

		compressed = snappy.Encode(nil, data)
		if _, err := w.Write(compressed); err != nil {
			_ = level.Warn(logger).Log("msg", "Error writing response", "storage", reader.Name(), "err", err)
		}
	})

	return http.ListenAndServe(addr, nil)
}

func protoToSamples(req *prompb.WriteRequest) model.Samples {
	var samples model.Samples
	for _, ts := range req.Timeseries {
		metric := make(model.Metric, len(ts.Labels))
		for _, l := range ts.Labels {
			metric[model.LabelName(l.Name)] = model.LabelValue(l.Value)
		}

		for _, s := range ts.Samples {
			samples = append(samples, &model.Sample{
				Metric:    metric,
				Value:     model.SampleValue(s.Value),
				Timestamp: model.Time(s.Timestamp),
			})
		}
	}
	return samples
}

func sendSamples(logger log.Logger, w logWriter, samples model.Samples) {
	begin := time.Now()
	err := w.Write(samples)
	duration := time.Since(begin).Seconds()
	if err != nil {
		_ = level.Warn(logger).Log("msg", "Error sending samples to remote storage", "err", err, "storage", w.Name(), "num_samples", len(samples))
		failedSamples.WithLabelValues(w.Name()).Add(float64(len(samples)))
	}
	sentSamples.WithLabelValues(w.Name()).Add(float64(len(samples)))
	sentBatchDuration.WithLabelValues(w.Name()).Observe(duration)
}
