package plan

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MetricsStoreConfig is the metrics_store block written into the server
// block of every server: the history of the task group usage, the cluster
// capacity and the control plane that each server keeps. The same values on
// every server keep the history of the servers alike.
type MetricsStoreConfig struct {
	// Retention is how long the history is kept: 1h to 30d, as a duration
	// ("168h") or in days ("7d"). Empty keeps the default of Norca (30d).
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"`
	// MaxDiskSize caps the disk use of the store on each server ("2GiB",
	// "512MiB"); the oldest points go first. Empty keeps the default of
	// Norca (2GiB).
	MaxDiskSize string `yaml:"max_disk_size,omitempty" json:"max_disk_size,omitempty"`
}

// The retention range of the metrics store of Norca.
const (
	MinMetricsRetention = time.Hour
	MaxMetricsRetention = 30 * 24 * time.Hour
)

// metricsRetentionDays matches a retention that starts with whole days.
var metricsRetentionDays = regexp.MustCompile(`^([0-9]+)d(.*)$`)

// ParseMetricsRetention parses a retention like Norca does: a Go duration
// ("168h", "90m") or whole days, optionally followed by a Go duration ("7d",
// "1d12h").
func ParseMetricsRetention(s string) (time.Duration, error) {
	m := metricsRetentionDays.FindStringSubmatch(s)
	if m == nil {
		return time.ParseDuration(s)
	}
	days, err := strconv.ParseInt(m[1], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	d := time.Duration(days) * 24 * time.Hour
	if m[2] != "" {
		rest, err := time.ParseDuration(m[2])
		if err != nil || rest < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d += rest
	}
	return d, nil
}

// byteSize matches a size: a number and an optional unit (B, kB/KiB to
// EB/EiB, the "B" optional), as Norca reads it.
var byteSize = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(?:([kmgtpe])(i)?b?|b)?$`)

// ParseByteSize parses a size such as "2GiB", "512MiB", "2GB" or "1048576"
// into bytes: the units with "i" are powers of 1024, the others of 1000.
func ParseByteSize(s string) (int64, error) {
	m := byteSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	base := 1000.0
	if m[3] != "" {
		base = 1024
	}
	if m[2] != "" {
		for range strings.Index("kmgtpe", strings.ToLower(m[2])) + 1 {
			n *= base
		}
	}
	if n >= 1<<63 {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return int64(n), nil
}

// validateMetricsStore mirrors the checks of the Norca server, which refuses
// to start with an invalid metrics_store block.
func (p *DeploymentPlan) validateMetricsStore() error {
	m := p.MetricsStore
	if m == nil {
		return nil
	}
	if m.Retention != "" {
		d, err := ParseMetricsRetention(m.Retention)
		if err != nil {
			return fmt.Errorf("metrics_store.retention: %q is not a duration (\"168h\") or a number of days (\"7d\")", m.Retention)
		}
		if d < MinMetricsRetention || d > MaxMetricsRetention {
			return fmt.Errorf("metrics_store.retention must be between 1h and 720h (30d), not %s", m.Retention)
		}
	}
	if m.MaxDiskSize != "" {
		if _, err := ParseByteSize(m.MaxDiskSize); err != nil {
			return fmt.Errorf("metrics_store.max_disk_size: %q is not a size (\"2GiB\", \"512MiB\")", m.MaxDiskSize)
		}
	}
	return nil
}
