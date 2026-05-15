package traceenrichmentprocessor

import (
	"errors"
	"time"
)

// Config holds the configuration for the traceenrichment processor.
type Config struct {
	Cache CacheConfig `mapstructure:"cache"`

	// ContainerIDAttribute is the resource attribute on incoming spans
	// that holds the container ID. Set by k8sattributesprocessor upstream.
	ContainerIDAttribute string `mapstructure:"container_id_attribute"`

	// TetragonContainerIDSubpath is the dot-separated path within a
	// Tetragon event-type attribute map to the container ID field.
	// The processor tries this subpath under every top-level attribute Map,
	// so it works for process_exec, process_exit, process_kprobe, etc.
	// Default: "process.pod.container.id"
	TetragonContainerIDSubpath string `mapstructure:"tetragon_container_id_subpath"`

	// EnrichmentAttributes lists which span attributes are copied onto
	// the matched log record. Locked outputs: trace_id, span_id, service.name.
	EnrichmentAttributes []string `mapstructure:"enrichment_attributes"`

	// MissMarkerAttribute is the boolean attribute set on log records for
	// which no cache entry was found.
	MissMarkerAttribute string `mapstructure:"miss_marker_attribute"`

	// MatchTimestampSkew is the symmetric tolerance applied when checking
	// whether a log timestamp falls inside a cached span's active window.
	// Accounts for clock drift between kernel monotonic and SDK wall clocks.
	MatchTimestampSkew time.Duration `mapstructure:"match_timestamp_skew"`
}

// CacheConfig controls the in-memory span cache.
type CacheConfig struct {
	// TTL is how long a span entry remains eligible for enrichment.
	TTL time.Duration `mapstructure:"ttl"`

	// MaxEntries is the hard cap on cache size. Oldest entries evicted first.
	MaxEntries int `mapstructure:"max_entries"`

	// CleanupInterval is how often the background eviction sweep runs.
	CleanupInterval time.Duration `mapstructure:"cleanup_interval"`
}

func defaultConfig() *Config {
	return &Config{
		Cache: CacheConfig{
			TTL:             30 * time.Second,
			MaxEntries:      100000,
			CleanupInterval: 5 * time.Second,
		},
		ContainerIDAttribute:       "container.id",
		TetragonContainerIDSubpath: "process.pod.container.id",
		EnrichmentAttributes:       []string{"trace_id", "span_id", "service.name"},
		MissMarkerAttribute:        "correlation_miss",
		MatchTimestampSkew:         500 * time.Millisecond,
	}
}

func (c *Config) Validate() error {
	if c.Cache.TTL <= 0 {
		return errors.New("cache.ttl must be positive")
	}
	if c.Cache.MaxEntries <= 0 {
		return errors.New("cache.max_entries must be positive")
	}
	if c.Cache.CleanupInterval <= 0 {
		return errors.New("cache.cleanup_interval must be positive")
	}
	if c.ContainerIDAttribute == "" {
		return errors.New("container_id_attribute must not be empty")
	}
	if c.TetragonContainerIDSubpath == "" {
		return errors.New("tetragon_container_id_subpath must not be empty")
	}
	if len(c.EnrichmentAttributes) == 0 {
		return errors.New("enrichment_attributes must not be empty")
	}
	if c.MissMarkerAttribute == "" {
		return errors.New("miss_marker_attribute must not be empty")
	}
	return nil
}
