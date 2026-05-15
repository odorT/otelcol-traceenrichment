package traceenrichmentprocessor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.uber.org/zap"
)

func noopMeter() metric.Meter { return noop.NewMeterProvider().Meter("test") }

func newTestLogsProcessor(t *testing.T, cache *spanCache) (*logsProcessor, *consumertest.LogsSink) {
	t.Helper()
	meter := noopMeter()
	sink := &consumertest.LogsSink{}
	eventsTotal, _ := meter.Int64Counter("events_total")
	lookupDuration, _ := meter.Float64Histogram("lookup_duration")
	return &logsProcessor{
		cfg:              defaultConfig(),
		cache:            cache,
		next:             sink,
		logger:           zap.NewNop(),
		eventsTotal:      eventsTotal,
		lookupDuration:   lookupDuration,
		warnedContainers: make(map[string]struct{}),
	}, sink
}

func newTestTracesProcessor(t *testing.T, cache *spanCache) (*tracesProcessor, *consumertest.TracesSink) {
	t.Helper()
	meter := noopMeter()
	sink := &consumertest.TracesSink{}
	spansObserved, _ := meter.Int64Counter("spans_observed")
	spansSkipped, _ := meter.Int64Counter("spans_skipped")
	return &tracesProcessor{
		cfg:           defaultConfig(),
		cache:         cache,
		next:          sink,
		logger:        zap.NewNop(),
		spansObserved: spansObserved,
		spansSkipped:  spansSkipped,
	}, sink
}

// --- logs processor: enrichment hit ---

func TestLogsProcessor_EnrichmentHit(t *testing.T) {
	cache := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	now := time.Now()
	containerID := "abc123def456"

	cache.put(containerID, spanEntry{
		TraceID:     makeTraceID(0xAA),
		SpanID:      makeSpanID(0xBB),
		ServiceName: "frontend",
		StartTime:   now,
		SeenAt:      now,
	})

	lp, sink := newTestLogsProcessor(t, cache)

	ld := buildLogWithContainerID(t, "process_exec", containerID, now)
	require.NoError(t, lp.ConsumeLogs(context.Background(), ld))
	require.Len(t, sink.AllLogs(), 1)

	rec := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	traceID, ok := rec.Attributes().Get("trace_id")
	require.True(t, ok)
	assert.Equal(t, traceIDToHex(makeTraceID(0xAA)), traceID.Str())

	spanID, ok := rec.Attributes().Get("span_id")
	require.True(t, ok)
	assert.Equal(t, spanIDToHex(makeSpanID(0xBB)), spanID.Str())

	svc, ok := rec.Attributes().Get("service.name")
	require.True(t, ok)
	assert.Equal(t, "frontend", svc.Str())

	hitAttr, hasHit := rec.Attributes().Get("correlation_hit")
	require.True(t, hasHit)
	assert.True(t, hitAttr.Bool())

	_, hasMiss := rec.Attributes().Get("correlation_miss")
	assert.False(t, hasMiss)
}

// --- logs processor: miss (no active span) ---

func TestLogsProcessor_MissNoActiveSpan(t *testing.T) {
	cache := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	lp, sink := newTestLogsProcessor(t, cache)

	ld := buildLogWithContainerID(t, "process_exec", "unknownctr", time.Now())
	require.NoError(t, lp.ConsumeLogs(context.Background(), ld))
	require.Len(t, sink.AllLogs(), 1)

	rec := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	miss, ok := rec.Attributes().Get("correlation_miss")
	require.True(t, ok)
	assert.True(t, miss.Bool())

	_, hasTraceID := rec.Attributes().Get("trace_id")
	assert.False(t, hasTraceID)
}

// --- logs processor: no container ID ---

func TestLogsProcessor_MissNoContainerID(t *testing.T) {
	cache := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	lp, sink := newTestLogsProcessor(t, cache)

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	sl := rl.ScopeLogs().AppendEmpty()
	rec := sl.LogRecords().AppendEmpty()
	rec.Attributes().PutStr("node_name", "some-node") // no event type map

	require.NoError(t, lp.ConsumeLogs(context.Background(), ld))
	rec2 := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	miss, ok := rec2.Attributes().Get("correlation_miss")
	require.True(t, ok)
	assert.True(t, miss.Bool())
	reason, _ := rec2.Attributes().Get("correlation_miss_reason")
	assert.Equal(t, "no_container_id", reason.Str())
}

// --- logs processor: container ID normalisation applied on lookup ---

func TestLogsProcessor_ContainerIDNormalised(t *testing.T) {
	cache := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	now := time.Now()
	bareID := "deadbeef0123"
	cache.put(bareID, spanEntry{
		TraceID:   makeTraceID(0x11),
		StartTime: now,
		SeenAt:    now,
	})

	lp, sink := newTestLogsProcessor(t, cache)
	// Log carries the containerd:// prefixed form
	ld := buildLogWithContainerID(t, "process_exec", "containerd://"+bareID, now)
	require.NoError(t, lp.ConsumeLogs(context.Background(), ld))

	rec := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	traceID, ok := rec.Attributes().Get("trace_id")
	require.True(t, ok)
	assert.Equal(t, traceIDToHex(makeTraceID(0x11)), traceID.Str())

	hitAttr, hasHit := rec.Attributes().Get("correlation_hit")
	require.True(t, hasHit)
	assert.True(t, hitAttr.Bool())
}

// --- traces processor: populates cache ---

func TestTracesProcessor_PopulatesCache(t *testing.T) {
	cache := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	tp, _ := newTestTracesProcessor(t, cache)

	td := buildTrace(t, "mycontainer", "checkout")
	require.NoError(t, tp.ConsumeTraces(context.Background(), td))

	_, ok := cache.lookup("mycontainer", time.Now())
	assert.True(t, ok)
}

func TestTracesProcessor_SkipsWhenNoContainerID(t *testing.T) {
	cache := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	tp, _ := newTestTracesProcessor(t, cache)

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Spans().AppendEmpty() // no container.id on resource

	require.NoError(t, tp.ConsumeTraces(context.Background(), td))
	assert.Equal(t, 0, cache.size())
}

// --- helpers ---

// buildLogWithContainerID creates a log record with a Tetragon-style
// attribute map (eventType -> Map -> process -> pod -> container -> id).
func buildLogWithContainerID(t *testing.T, eventType string, containerID string, ts time.Time) plog.Logs {
	t.Helper()
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	sl := rl.ScopeLogs().AppendEmpty()
	rec := sl.LogRecords().AppendEmpty()
	rec.SetTimestamp(pcommon.NewTimestampFromTime(ts))

	eventMap := rec.Attributes().PutEmptyMap(eventType)
	processMap := eventMap.PutEmptyMap("process")
	podMap := processMap.PutEmptyMap("pod")
	containerMap := podMap.PutEmptyMap("container")
	containerMap.PutStr("id", containerID)

	return ld
}

func buildTrace(t *testing.T, containerID, serviceName string) ptrace.Traces {
	t.Helper()
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("container.id", containerID)
	rs.Resource().Attributes().PutStr("service.name", serviceName)
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(50 * time.Millisecond)))
	return td
}
