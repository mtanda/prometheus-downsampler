package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"gopkg.in/yaml.v2"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	l "github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	v "github.com/prometheus/prometheus/pkg/value"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/wal"
)

const (
	downsampleInterval        = 60 * 60 // 1 hour
	timerInterval             = downsampleInterval * time.Second
	fetchDurationMilliseconds = downsampleInterval / 2 * 1000
	maxRetryCount             = 7
)

var (
	lastSuccess = prometheus.NewGaugeVec(
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

type DownsampleConfig struct {
	DownsampleTypes []string `yaml:"downsample_types"`
	IgnorePatterns  []string `yaml:"ignore_patterns"`
}

type SelfMetricsConfig struct {
	TextfileCollectorOutput string `yaml:"textfile_collector_output"`
}

type Config struct {
	DownsampleConfig  DownsampleConfig  `yaml:"downsample_config"`
	RelabelConfig     []*relabel.Config `yaml:"relabel_config"`
	ExcludeLabel      string            `yaml:"exclude_label"`
	SelfMetricsConfig SelfMetricsConfig `yaml:"self_metrics_config"`
}

type Downsampler struct {
	promAddr    string
	tmpDbPath   string
	dbPath      string
	db          *tsdb.DB
	config      *Config
	maxSamples  int
	ignoreCache map[string]bool
	mss         []*tsdb.MetricSample
	minTime     int64
	maxTime     int64
	logger      *log.Logger
	enableSleep bool
}

func (d *Downsampler) isIgnoreMetrics(metricName string) bool {
	if c, ok := d.ignoreCache[metricName]; ok {
		return c
	}

	for _, p := range d.config.DownsampleConfig.IgnorePatterns {
		matched, err := regexp.Match(p, []byte(metricName))
		if err != nil {
			// ignore error
			continue
		}
		if !d.ignoreCache[metricName] {
			d.ignoreCache[metricName] = matched
		}
		if matched {
			break
		}
	}

	return d.ignoreCache[metricName]
}

func (d *Downsampler) createBlock() (string, error) {
	blockID, err := tsdb.CreateBlock(d.mss, d.tmpDbPath, d.minTime, d.maxTime, *d.logger)
	if err != nil {
		return "", err
	}
	bid := filepath.Base(blockID)
	fromPath := d.tmpDbPath + "/" + bid
	toPath := d.dbPath + "/" + bid
	if err := os.Rename(fromPath, toPath+".tmp"); err != nil {
		return "", err
	}
	if err := os.Rename(toPath+".tmp", toPath); err != nil {
		return "", err
	}
	level.Info(*d.logger).Log("msg", "move block to data directory", "from", fromPath, "to", toPath)

	d.minTime = math.MaxInt64
	d.maxTime = math.MinInt64
	d.mss = d.mss[:0]
	return blockID, nil
}

func (d *Downsampler) downsample() error {
	ctx := context.Background()
	downsampleTypes := d.config.DownsampleConfig.DownsampleTypes
	if len(downsampleTypes) == 0 {
		downsampleTypes = []string{"avg", "max"}
	}
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

	var version v1.BuildinfoResult
	version, err = promAPI.Buildinfo(ctx)
	if err != nil {
		return err
	}
	var downsampleDuration string
	if strings.HasPrefix(version.Version, "2.") {
		downsampleDuration = strconv.FormatInt(int64(downsampleInterval-1), 10) + "s"
	} else if strings.HasPrefix(version.Version, "3.") {
		downsampleDuration = strconv.FormatInt(int64(downsampleInterval), 10) + "s"
	} else {
		return fmt.Errorf("Prometheus version is not supported")
	}

	progressTotal := len(lr.Data) * len(downsampleTypes)
	sleepDuration := time.Duration(fetchDurationMilliseconds/progressTotal) * time.Millisecond
	processed.WithLabelValues().Set(0)
	d.minTime = math.MaxInt64
	d.maxTime = math.MinInt64

	downsampleLabel := "__meta_downsampler_downsample_type"
	relabelConfig := d.config.RelabelConfig
	relabelConfig = append(relabelConfig, &relabel.Config{Regex: relabel.MustNewRegexp("^" + downsampleLabel + "$"), Action: "labeldrop"})

	for _, metricName := range lr.Data {
		for _, downsampleType := range downsampleTypes {
			if d.isIgnoreMetrics(metricName) {
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
				if len(allMetadata) == 0 || (allMetadata[metricName][0].Type != v1.MetricTypeCounter && allMetadata[metricName][0].Type != v1.MetricTypeUnknown) {
					query = downsampleType + "_over_time({__name__=\"" + metricName + "\"}[" + downsampleDuration + "])"
				} else if allMetadata[metricName][0].Type == v1.MetricTypeCounter {
					if downsampleType != "max" {
						query = "increase({__name__=\"" + metricName + "\"}[" + downsampleDuration + "])"
					} else {
						query = "max_over_time(rate({__name__=\"" + metricName + "\"}[30s])[" + downsampleDuration + ":15s])"
					}
				} else if allMetadata[metricName][0].Type == v1.MetricTypeUnknown {
					if downsampleType != "max" {
						query = "last_over_time({__name__=\"" + metricName + "\"}[" + downsampleDuration + "])"
					} else {
						query = "max_over_time({__name__=\"" + metricName + "\"}[" + downsampleDuration + "])"
					}
				}
				value, warn, err = promAPI.Query(ctx, query, downsampleBaseTimestamp)
				isSuccess := false
				if warn == nil && err == nil {
					isSuccess = true
				}
				for _, w := range warn {
					if strings.Contains(w, "metric might not be a counter, name does not end in") {
						isSuccess = true
					}
				}
				if isSuccess {
					break
				}
				retryWait := time.Duration(math.Pow(2, float64(retryCount)))
				level.Info(*d.logger).Log("query", query, "retry", retryCount, "wait", retryWait, "error", err, "warn", warn)
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
				lb.Set(l.MetricName, metricName)
				lb.Set(downsampleLabel, downsampleType)
				ls := relabel.Process(lb.Labels(), relabelConfig...)

				if ls.Get(d.config.ExcludeLabel) != "" {
					continue
				}

				if len(allMetadata) == 0 || allMetadata[metricName][0].Type != v1.MetricTypeCounter || downsampleType == "max" {
					d.mss = append(d.mss, &tsdb.MetricSample{Labels: ls, Value: float64(m.Value), TimestampMs: (m.Timestamp.Unix() - downsampleInterval + 1) * 1000})
				} else {
					d.mss = append(d.mss, &tsdb.MetricSample{Labels: ls, Value: float64(0), TimestampMs: (m.Timestamp.Unix() - downsampleInterval + 1) * 1000})
				}
				d.minTime = min(d.minTime, (m.Timestamp.Unix()-downsampleInterval+1)*1000)
				d.maxTime = max(d.maxTime, (m.Timestamp.Unix()-downsampleInterval+1)*1000)
				d.mss = append(d.mss, &tsdb.MetricSample{Labels: ls, Value: float64(m.Value), TimestampMs: m.Timestamp.Unix() * 1000})
				d.minTime = min(d.minTime, m.Timestamp.Unix()*1000)
				d.maxTime = max(d.maxTime, m.Timestamp.Unix()*1000)
				d.mss = append(d.mss, &tsdb.MetricSample{Labels: ls, Value: math.Float64frombits(v.StaleNaN), TimestampMs: (m.Timestamp.Unix() + 1) * 1000})
				d.minTime = min(d.minTime, (m.Timestamp.Unix()+1)*1000)
				d.maxTime = max(d.maxTime, (m.Timestamp.Unix()+1)*1000)

				if len(d.mss) == d.maxSamples {
					blockID, err := d.createBlock()
					if err != nil {
						return err
					}
					level.Info(*d.logger).Log("msg", "create block successfully", "block", blockID)
				}
			}

			if d.enableSleep {
				time.Sleep(sleepDuration)
			}
		}
		processed.WithLabelValues().Inc()
	}

	if len(d.mss) > 0 {
		blockID, err := d.createBlock()
		if err != nil {
			return err
		}
		level.Info(*d.logger).Log("msg", "create block successfully", "block", blockID)
	}

	lastSuccess.WithLabelValues().Set(float64(time.Now().Unix()))
	return nil
}

func exportMetrics(filename string, registry *prometheus.Registry) error {
	metricsFamilies, err := registry.Gather()
	if err != nil {
		return err
	}

	tempFilename := filename + ".tmp"
	file, err := os.Create(tempFilename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := expfmt.NewEncoder(file, expfmt.FmtText)
	for _, mf := range metricsFamilies {
		if err := encoder.Encode(mf); err != nil {
			return err
		}
	}

	return os.Rename(tempFilename, filename)
}

func LoadConfig(configFile string) (*Config, error) {
	var cfg Config
	if len(configFile) == 0 {
		return &cfg, nil
	}

	buf, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(buf, &cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}

func main() {
	var promAddr string
	var listenAddr string
	var tmpDbPath string
	var dbPath string
	var configFile string
	var maxSamples int
	flag.StringVar(&promAddr, "prometheus.addr", "http://127.0.0.1:9090/", "Prometheus address to aggregate metrics")
	flag.StringVar(&listenAddr, "web.listen-address", ":19200", "Address on which to expose metrics")
	flag.StringVar(&tmpDbPath, "tsdb.tmp-path", "./data.tmp", "Prometheus TSDB temporary data directory")
	flag.StringVar(&dbPath, "tsdb.path", "./data", "Prometheus TSDB data directory")
	flag.StringVar(&configFile, "config.file", "", "Configuration file path.")
	flag.IntVar(&maxSamples, "max-samples", 10000000, "Maximum number of samples")
	flag.Parse()

	logLevel := promlog.AllowedLevel{}
	logLevel.Set("info")
	format := promlog.AllowedFormat{}
	format.Set("json")
	logCfg := promlog.Config{Level: &logLevel, Format: &format}
	logger := promlog.New(&logCfg)

	cfg, err := LoadConfig(configFile)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
	tmpDbPath, err = filepath.Abs(tmpDbPath)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
	dbPath, err = filepath.Abs(dbPath)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
	err = os.Mkdir(dbPath, 0755)
	if err != nil && !os.IsExist(err) {
		level.Error(logger).Log("err", err)
		panic(err)
	}

	opts := &tsdb.Options{
		WALSegmentSize: wal.DefaultSegmentSize,
		NoLockfile:     true,
	}
	db, err := tsdb.Open(tmpDbPath, logger, prometheus.DefaultRegisterer, opts)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
	defer db.Close()

	var wg sync.WaitGroup
	stopper := make(chan struct{})
	if cfg.SelfMetricsConfig.TextfileCollectorOutput != "" {
		go func() {
			registry := prometheus.NewRegistry()
			registry.MustRegister(lastSuccess)
			registry.MustRegister(processed)
			registry.MustRegister(retryTotal)
			registry.MustRegister(failureTotal)

			ticker := time.NewTicker(time.Second * 5)
			defer ticker.Stop()

			wg.Add(1)
		L:
			for {
				select {
				case <-ticker.C:
					exportMetrics(cfg.SelfMetricsConfig.TextfileCollectorOutput, registry)
				case <-stopper:
					exportMetrics(cfg.SelfMetricsConfig.TextfileCollectorOutput, registry)
					break L
				}
			}
			wg.Done()
		}()
	}

	d := Downsampler{
		promAddr:    promAddr,
		tmpDbPath:   tmpDbPath,
		dbPath:      dbPath,
		db:          db,
		config:      cfg,
		maxSamples:  maxSamples,
		ignoreCache: make(map[string]bool),
		logger:      &logger,
		enableSleep: true,
	}
	level.Info(logger).Log("msg", "downsample start")
	if err := d.downsample(); err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
	close(stopper)
	wg.Wait()
	level.Info(logger).Log("msg", "downsample done")
}
