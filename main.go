package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	l "github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/wal"
)

const (
	downsampleInterval        = 60 * 60 // 1 hour
	timerInterval             = downsampleInterval * time.Second
	fetchDurationMilliseconds = downsampleInterval / 2 * 1000
	maxSamples                = 100
	maxRetryCount             = 7
	enableSleep               = true
)

var (
	downsampleDuration = strconv.FormatInt(int64(downsampleInterval), 10) + "s"
	lastSuccess        = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "downsampler_last_success_timestamp_seconds",
			Help: "The last success timestamp",
		},
		[]string{},
	)
	processed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "downsampler_processed_series",
			Help: "The number of processed_series",
		},
		[]string{},
	)
	retryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "downsampler_retry_total",
			Help: "The total number of retry",
		},
		[]string{},
	)
	failureTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "downsampler_failure_total",
			Help: "The total number of failure",
		},
		[]string{},
	)
)

type LabelResponse struct {
	Status string
	Data   []string
}

type Metrics struct {
	Metric map[string]string
	Value  interface{}
}

type MetricsData struct {
	ResultType string
	Result     []Metrics
}

type MetricsResponse struct {
	Status string
	Data   MetricsData
}

var ignoreCache = make(map[string]bool)

func isIgnoreMetrics(metricName string) bool {
	// TODO: read from config
	ignoreCache[metricName] = false
	return false
}

func getLabels(path string, label string, lr *LabelResponse) error {
	resp, err := http.Get(path + "api/v1/label/" + label + "/values")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return errors.New("label API request error")
	}

	err = json.NewDecoder(resp.Body).Decode(&lr)
	if err != nil {
		return err
	}
	if len(lr.Data) == 0 {
		return errors.New("labels is empty")
	}

	return nil
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b int64) int64 {
	if a > b {
		return b
	}
	return a
}

type Downsampler struct {
	promAddr string
	dbPath   string
	db       *tsdb.DB
	logger   *log.Logger
}

func (d *Downsampler) downsample() error {
	ctx := context.Background()
	aggregationTypes := []string{"avg", "max"}
	now := time.Now().UTC()
	downsampleBaseTimestamp := now.Truncate(timerInterval)
	var retryCount int

	var lr LabelResponse
	for retryCount = 0; retryCount <= maxRetryCount; retryCount++ {
		err := getLabels(d.promAddr, "__name__", &lr)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if retryCount > maxRetryCount {
		return fmt.Errorf("get labels error")
	}

	var config api.Config
	config.Address = d.promAddr
	var client api.Client
	var err error
	client, err = api.NewClient(config)
	if err != nil {
		return err
	}
	promAPI := v1.NewAPI(client)

	progressTotal := len(lr.Data) * len(aggregationTypes)
	sleepDuration := time.Duration(fetchDurationMilliseconds/progressTotal) * time.Millisecond
	processed.WithLabelValues().Set(0)
	var mss []*tsdb.MetricSample
	var minTime int64 = math.MaxInt64
	var maxTime int64 = math.MinInt64

	for _, metricName := range lr.Data {
		for _, aggregationType := range aggregationTypes {
			if isIgnoreMetrics(metricName) {
				continue
			}

			var value model.Value
			var allMetadata map[string][]v1.Metadata
			var query string
			for retryCount = 0; retryCount <= maxRetryCount; retryCount++ {
				var warn v1.Warnings
				allMetadata, err = promAPI.Metadata(ctx, metricName, "1")
				if err != nil {
					level.Error(*d.logger).Log("error", err)
					continue
				}
				if len(allMetadata) == 0 || allMetadata[metricName][0].Type != v1.MetricTypeCounter {
					query = aggregationType + "_over_time({__name__=\"" + metricName + "\"}[" + downsampleDuration + "])"
				} else {
					query = "increase({__name__=\"" + metricName + "\"}[" + downsampleDuration + "])"
				}
				value, warn, err = promAPI.Query(ctx, query, downsampleBaseTimestamp)
				if warn == nil && err == nil {
					break
				}
				retryWait := time.Duration(math.Pow(2, float64(retryCount)))
				level.Info(*d.logger).Log("query", query, "retry", retryCount, "wait", retryWait)
				time.Sleep(retryWait * time.Second)
				retryTotal.WithLabelValues().Inc()
			}
			if retryCount > maxRetryCount {
				level.Error(*d.logger).Log("query", query, "error", err)
				failureTotal.WithLabelValues().Inc()
				continue
			}

			for _, m := range value.(model.Vector) {
				lb := l.NewBuilder(make(l.Labels, 0))
				for n, v := range m.Metric {
					lb.Set(string(n), string(v))
				}
				lb.Set(l.MetricName, metricName+"_"+aggregationType) // assume __name__ is not set

				if len(allMetadata) == 0 || allMetadata[metricName][0].Type != v1.MetricTypeCounter {
					mss = append(mss, &tsdb.MetricSample{Labels: lb.Labels(), Value: float64(m.Value), TimestampMs: (m.Timestamp.Unix() - downsampleInterval + 1) * 1000})
				} else {
					mss = append(mss, &tsdb.MetricSample{Labels: lb.Labels(), Value: float64(0), TimestampMs: (m.Timestamp.Unix() - downsampleInterval + 1) * 1000})
				}
				minTime = min(minTime, (m.Timestamp.Unix()-downsampleInterval+1)*1000)
				maxTime = max(maxTime, (m.Timestamp.Unix()-downsampleInterval+1)*1000)
				mss = append(mss, &tsdb.MetricSample{Labels: lb.Labels(), Value: float64(m.Value), TimestampMs: m.Timestamp.Unix() * 1000})
				minTime = min(minTime, m.Timestamp.Unix()*1000)
				maxTime = max(maxTime, m.Timestamp.Unix()*1000)

				if len(mss) == maxSamples {
					blockID, err := tsdb.CreateBlock(mss, d.dbPath, minTime, maxTime, *d.logger)
					if err != nil {
						return err
					}

					minTime = math.MaxInt64
					maxTime = math.MinInt64
					mss = mss[:0]
					level.Info(*d.logger).Log("msg", "create block successfully", "block", blockID)
				}
			}

			if enableSleep {
				time.Sleep(sleepDuration)
			}
		}
		processed.WithLabelValues().Inc()
	}

	if len(mss) > 0 {
		blockID, err := tsdb.CreateBlock(mss, d.dbPath, minTime, maxTime, *d.logger)
		if err != nil {
			return err
		}
		level.Info(*d.logger).Log("msg", "create block successfully", "block", blockID)
	}

	lastSuccess.WithLabelValues().Set(float64(time.Now().Unix()))
	return nil
}

func main() {
	var promAddr string
	var dbPath string
	flag.StringVar(&promAddr, "prometheus.addr", "http://127.0.0.1:9090/", "Prometheus address to aggregate metrics")
	flag.StringVar(&dbPath, "tsdb.path", "./data", "Prometheus TSDB data directory")
	flag.Parse()

	logLevel := promlog.AllowedLevel{}
	logLevel.Set("info")
	format := promlog.AllowedFormat{}
	format.Set("json")
	logCfg := promlog.Config{Level: &logLevel, Format: &format}
	logger := promlog.New(&logCfg)

	var err error
	dbPath, err = filepath.Abs(dbPath)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}

	opts := &tsdb.Options{
		WALSegmentSize: wal.DefaultSegmentSize,
		NoLockfile:     true,
	}
	db, err := tsdb.Open(dbPath, logger, prometheus.DefaultRegisterer, opts)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
	defer db.Close()

	now := time.Now()
	t := time.NewTimer(timerInterval - (now.Sub(now.Truncate(timerInterval))))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			t.Reset(timerInterval)

			d := Downsampler{
				promAddr: promAddr,
				dbPath:   dbPath,
				db:       db,
				logger:   &logger,
			}
			time.Sleep(60 * time.Second)
			level.Info(logger).Log("msg", "downsample start")
			if err := d.downsample(); err != nil {
				level.Error(logger).Log("err", err)
				panic(err)
			}
			level.Info(logger).Log("msg", "downsample done")
		}
	}
}
