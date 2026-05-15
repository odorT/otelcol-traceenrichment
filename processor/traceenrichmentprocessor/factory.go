package traceenrichmentprocessor

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const typeStr = "traceenrichment"

var typ = component.MustNewType(typeStr)

// sharedState holds the cache and eviction goroutine shared between the
// traces processor and the logs processor for the same config ID.
// Both pipelines must reference the same processor name in collector config.
type sharedState struct {
	cache      *spanCache
	stopCh     chan struct{}
	ttlEvicted atomic.Int64
	maxEvicted atomic.Int64
}

type processorFactory struct {
	mu     sync.Mutex
	states map[component.ID]*sharedState
}

// NewFactory returns the processor.Factory for the traceenrichment processor.
func NewFactory() processor.Factory {
	f := &processorFactory{
		states: make(map[component.ID]*sharedState),
	}
	return processor.NewFactory(
		typ,
		func() component.Config { return defaultConfig() },
		processor.WithTraces(f.createTracesProcessor, component.StabilityLevelAlpha),
		processor.WithLogs(f.createLogsProcessor, component.StabilityLevelAlpha),
	)
}

func (f *processorFactory) getOrCreate(id component.ID, cfg *Config) *sharedState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.states[id]; ok {
		return s
	}
	c := newSpanCache(cfg.Cache, cfg.MatchTimestampSkew)
	s := &sharedState{
		cache:  c,
		stopCh: make(chan struct{}),
	}
	go func() {
		ticker := time.NewTicker(cfg.Cache.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ttl, max := c.evict()
				s.ttlEvicted.Add(int64(ttl))
				s.maxEvicted.Add(int64(max))
			case <-s.stopCh:
				return
			}
		}
	}()
	f.states[id] = s
	return s
}

func (f *processorFactory) createTracesProcessor(
	_ context.Context,
	set processor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (processor.Traces, error) {
	c := cfg.(*Config)
	state := f.getOrCreate(set.ID, c)
	meter := set.TelemetrySettings.MeterProvider.Meter(typeStr)

	spansObserved, _ := meter.Int64Counter(
		"traceenrichment_spans_observed_total",
		metric.WithDescription("Total spans observed and indexed into the cache"),
	)
	spansSkipped, _ := meter.Int64Counter(
		"traceenrichment_spans_skipped_total",
		metric.WithDescription("Spans skipped due to missing container ID attribute"),
	)

	// Cache-size gauge: observed asynchronously
	cacheSize, _ := meter.Int64ObservableGauge(
		"traceenrichment_cache_size",
		metric.WithDescription("Number of container entries currently in the span cache"),
	)
	// Evictions counter: observed asynchronously from cumulative atomics on sharedState
	evictTotal, _ := meter.Int64ObservableCounter(
		"traceenrichment_cache_evictions_total",
		metric.WithDescription("Cache entries evicted, labeled by cause (ttl|max_entries)"),
	)
	meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(cacheSize, int64(state.cache.size()))
		o.ObserveInt64(evictTotal, state.ttlEvicted.Load(),
			metric.WithAttributes(attribute.String("cause", "ttl")))
		o.ObserveInt64(evictTotal, state.maxEvicted.Load(),
			metric.WithAttributes(attribute.String("cause", "max_entries")))
		return nil
	}, cacheSize, evictTotal)

	return &tracesProcessor{
		cfg:           c,
		cache:         state.cache,
		next:          next,
		logger:        set.Logger,
		spansObserved: spansObserved,
		spansSkipped:  spansSkipped,
	}, nil
}

func (f *processorFactory) createLogsProcessor(
	_ context.Context,
	set processor.Settings,
	cfg component.Config,
	next consumer.Logs,
) (processor.Logs, error) {
	c := cfg.(*Config)
	state := f.getOrCreate(set.ID, c)
	meter := set.TelemetrySettings.MeterProvider.Meter(typeStr)

	eventsTotal, _ := meter.Int64Counter(
		"traceenrichment_events_total",
		metric.WithDescription("Tetragon events processed, labeled by correlation result (enriched|miss_no_active_span|no_container_id)"),
	)
	lookupDuration, _ := meter.Float64Histogram(
		"traceenrichment_lookup_duration_seconds",
		metric.WithDescription("Time spent looking up a container ID in the cache"),
	)
	return &logsProcessor{
		cfg:              c,
		cache:            state.cache,
		next:             next,
		logger:           set.Logger,
		eventsTotal:      eventsTotal,
		lookupDuration:   lookupDuration,
		warnedContainers: make(map[string]struct{}),
	}, nil
}
