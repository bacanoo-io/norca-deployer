package plan

import (
	"strings"
	"testing"
	"time"
)

func TestParseMetricsRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"1h":    time.Hour,
		"168h":  168 * time.Hour,
		"7d":    7 * 24 * time.Hour,
		"30d":   MaxMetricsRetention,
		"1d12h": 36 * time.Hour,
	} {
		got, err := ParseMetricsRetention(in)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "7 days", "1.5d", "1d-1h", "week"} {
		if _, err := ParseMetricsRetention(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{
		"2GiB":    2 << 30,
		"512MiB":  512 << 20,
		"512 mib": 512 << 20,
		"2GB":     2_000_000_000,
		"1.5KiB":  1536,
		"1048576": 1 << 20,
		"10k":     10_000,
		"4Ki":     4096,
		"0":       0,
	} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("%s: got %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "lots", "2 GiBs", "-1GiB", "1x", "99999999EiB"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestValidateMetricsStore(t *testing.T) {
	for _, m := range []*MetricsStoreConfig{
		nil,
		{},
		{Retention: "1h"},
		{Retention: "7d", MaxDiskSize: "4GiB"},
		{Retention: "720h"},
		{MaxDiskSize: "512MiB"},
	} {
		p := validPlan()
		p.MetricsStore = m
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", m, err)
		}
	}
	for want, m := range map[string]*MetricsStoreConfig{
		"between 1h and 720h (30d), not 31d": {Retention: "31d"},
		"between 1h and 720h (30d), not 30m": {Retention: "30m"},
		"is not a duration":                  {Retention: "a week"},
		"max_disk_size":                      {MaxDiskSize: "big"},
	} {
		p := validPlan()
		p.MetricsStore = m
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: got %v, want an error containing %q", m, err, want)
		}
	}
}
