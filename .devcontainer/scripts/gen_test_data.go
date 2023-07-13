package main

import (
	"log"
	"os"
	"text/template"
	"time"
)

const (
	tmpl = `# TYPE test_gauge gauge
# HELP test_gauge Test gauge
{{ range $i, $v := .gauge_data -}}
test_gauge {{$v.Value}} {{$v.Timestamp}}
{{ end -}}
# TYPE test_counter_seconds counter
# UNIT test_counter_seconds seconds
# HELP test_counter_seconds Test counter
{{ range $i, $v := .counter_data -}}
test_counter_seconds_total {{$v.Value}} {{$v.Timestamp}}
{{ end -}}
# EOF
`
	truncateRange  = 1 * time.Hour
	backfillRange  = 2 * time.Hour
	scrapeInterval = 1 * time.Minute
)

type Datapoint struct {
	Timestamp int64
	Value     float64
}

func genGaugeData(now time.Time) []Datapoint {
	tp := now.Truncate(truncateRange).Add(-backfillRange).Unix()
	data := make([]Datapoint, 0)
	for n := 0; n < int(backfillRange.Seconds()/scrapeInterval.Seconds()); n++ {
		data = append(data, Datapoint{
			Timestamp: tp + int64(n*int(scrapeInterval.Seconds())),
			Value:     float64(n),
		})
	}
	return data
}

func genCounterData(now time.Time) []Datapoint {
	tp := now.Truncate(truncateRange).Add(-backfillRange).Unix()
	data := make([]Datapoint, 0)
	for n := 0; n < int(backfillRange.Seconds()/scrapeInterval.Seconds()); n++ {
		data = append(data, Datapoint{
			Timestamp: tp + int64(n*int(scrapeInterval.Seconds())),
			Value:     float64(n),
		})
	}
	return data
}

func main() {
	tpl, err := template.New("").Parse(tmpl)
	if err != nil {
		log.Fatal(err)
	}

	now := time.Now()
	m := map[string]interface{}{
		"gauge_data":   genGaugeData(now),
		"counter_data": genCounterData(now),
	}

	if err := tpl.Execute(os.Stdout, m); err != nil {
		log.Fatal(err)
	}
}
