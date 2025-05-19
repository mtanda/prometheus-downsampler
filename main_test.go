package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/wal"
)

func Test_downsample(t *testing.T) {
	promAddr := "http://prometheus:9090/"
	tmpDbPath := "/prometheus_downsample/data.tmp"
	dbPath := "/prometheus_downsample"
	maxSamples := 10000
	logger := log.NewNopLogger()

	opts := &tsdb.Options{
		WALSegmentSize: wal.DefaultSegmentSize,
		NoLockfile:     true,
	}
	tmpDb, err := tsdb.Open(tmpDbPath, logger, prometheus.NewRegistry(), opts)
	if err != nil {
		t.Fatalf("tsdb open error")
	}
	defer tmpDb.Close()

	d := Downsampler{
		promAddr:    promAddr,
		tmpDbPath:   tmpDbPath,
		dbPath:      dbPath,
		db:          tmpDb,
		config:      &Config{},
		maxSamples:  maxSamples,
		ignoreCache: make(map[string]bool),
		logger:      &logger,
		enableSleep: false,
	}
	if err := d.downsample(); err != nil {
		t.Errorf("down sample error")
	}

	downsampleClient, err := api.NewClient(api.Config{
		Address: "http://prometheus_downsample:9090/",
	})
	if err != nil {
		t.Fatalf("prometheus query error")
	}
	downsampleAPI := v1.NewAPI(downsampleClient)
	ctx := context.TODO()
	value, _, err := downsampleAPI.Query(ctx, "test_gauge", time.Now().Truncate(1*time.Hour))
	if err != nil {
		t.Fatalf("prometheus query error")
	}
	actual := value.(model.Vector)[0].Value
	expected := model.SampleValue(180)
	if actual != expected {
		t.Errorf("gauge downsampled value = %v, want %v", actual, expected)
	}
}

func TestMain(m *testing.M) {
	err := exec.Command("./.devcontainer/scripts/reset_data.sh").Run()
	if err != nil {
		panic("initialization error")
	}

	status := m.Run()

	os.Exit(status)
}
