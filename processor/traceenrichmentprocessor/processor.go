package traceenrichmentprocessor

import (
	"context"
	"encoding/hex"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
)

// tracesProcessor observes spans and populates the shared cache.
// It does NOT mutate spans — pure side-effect on the cache.
type tracesProcessor struct {
	cfg    *Config
	cache  *spanCache
	next   consumer.Traces
	logger *zap.Logger

	spansObserved metric.Int64Counter
	spansSkipped  metric.Int64Counter
}

var _ processor.Traces = (*tracesProcessor)(nil)

func (p *tracesProcessor) Start(_ context.Context, _ component.Host) error {
	p.logger.Info("traceenrichmentprocessor traces side started",
		zap.String("container_id_attribute", p.cfg.ContainerIDAttribute),
		zap.Duration("cache_ttl", p.cfg.Cache.TTL),
		zap.Int("cache_max_entries", p.cfg.Cache.MaxEntries),
	)
	return nil
}

func (p *tracesProcessor) Shutdown(_ context.Context) error { return nil }

func (p *tracesProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (p *tracesProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	for i := range td.ResourceSpans().Len() {
		rs := td.ResourceSpans().At(i)
		serviceName := resourceServiceName(rs.Resource())
		containerID := resourceContainerID(rs.Resource(), p.cfg.ContainerIDAttribute)

		if containerID == "" {
			p.spansSkipped.Add(ctx, int64(countSpans(rs)),
				metric.WithAttributes(attribute.String("reason", "no_container_id")))
			continue
		}

		normID := normalizeContainerID(containerID)

		spanCount := 0
		for j := range rs.ScopeSpans().Len() {
			ss := rs.ScopeSpans().At(j)
			for k := range ss.Spans().Len() {
				span := ss.Spans().At(k)
				p.cache.put(normID, spanEntry{
					TraceID:     span.TraceID(),
					SpanID:      span.SpanID(),
					ServiceName: serviceName,
					StartTime:   span.StartTimestamp().AsTime(),
					EndTime:     span.EndTimestamp().AsTime(),
					SeenAt:      time.Now(),
				})
				p.spansObserved.Add(ctx, 1)
				spanCount++
			}
		}
		p.logger.Debug("cached spans for container",
			zap.String("container_id_prefix", safePrefix(normID, 12)),
			zap.String("service", serviceName),
			zap.Int("spans", spanCount),
			zap.Int("cache_containers", p.cache.size()),
		)
	}
	return p.next.ConsumeTraces(ctx, td)
}

// logsProcessor enriches Tetragon log records with trace context from the cache.
type logsProcessor struct {
	cfg    *Config
	cache  *spanCache
	next   consumer.Logs
	logger *zap.Logger

	eventsTotal    metric.Int64Counter   // labeled by result
	lookupDuration metric.Float64Histogram

	warnedContainers map[string]struct{}
}

var _ processor.Logs = (*logsProcessor)(nil)

func (p *logsProcessor) Start(_ context.Context, _ component.Host) error {
	p.logger.Info("traceenrichmentprocessor logs side started",
		zap.String("tetragon_subpath", p.cfg.TetragonContainerIDSubpath),
		zap.String("miss_marker", p.cfg.MissMarkerAttribute),
		zap.Duration("match_skew", p.cfg.MatchTimestampSkew),
	)
	return nil
}

func (p *logsProcessor) Shutdown(_ context.Context) error { return nil }

func (p *logsProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

func (p *logsProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	for i := range ld.ResourceLogs().Len() {
		rl := ld.ResourceLogs().At(i)
		for j := range rl.ScopeLogs().Len() {
			sl := rl.ScopeLogs().At(j)
			for k := range sl.LogRecords().Len() {
				p.enrichRecord(ctx, sl.LogRecords().At(k))
			}
		}
	}
	return p.next.ConsumeLogs(ctx, ld)
}

func (p *logsProcessor) enrichRecord(ctx context.Context, rec plog.LogRecord) {
	containerID := p.extractContainerID(rec)
	if containerID == "" {
		rec.Attributes().PutBool(p.cfg.MissMarkerAttribute, true)
		rec.Attributes().PutStr(p.cfg.MissMarkerAttribute+"_reason", "no_container_id")
		p.eventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "no_container_id")))
		return
	}

	normID := normalizeContainerID(containerID)
	logTime := rec.Timestamp().AsTime()
	if logTime.IsZero() {
		logTime = rec.ObservedTimestamp().AsTime()
	}

	t0 := time.Now()
	entry, hit := p.cache.lookup(normID, logTime)
	resultLabel := "miss_no_active_span"
	if hit {
		resultLabel = "enriched"
	}
	p.lookupDuration.Record(ctx, time.Since(t0).Seconds(),
		metric.WithAttributes(attribute.String("result", resultLabel)))

	if hit {
		rec.Attributes().PutBool("correlation_hit", true)
		rec.Attributes().PutStr("trace_id", traceIDToHex(entry.TraceID))
		rec.Attributes().PutStr("span_id", spanIDToHex(entry.SpanID))
		rec.Attributes().PutStr("service.name", entry.ServiceName)
		p.eventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "enriched")))
		p.logger.Debug("enriched log record",
			zap.String("container_id_prefix", safePrefix(normID, 12)),
			zap.String("trace_id_suffix", traceIDToHex(entry.TraceID)[24:]),
		)
	} else {
		rec.Attributes().PutBool(p.cfg.MissMarkerAttribute, true)
		rec.Attributes().PutStr(p.cfg.MissMarkerAttribute+"_reason", "no_active_span")
		p.eventsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "miss_no_active_span")))
		if _, warned := p.warnedContainers[normID]; !warned {
			inCache := p.cache.contains(normID)
			fields := []zap.Field{
				zap.String("container_id_prefix", safePrefix(normID, 12)),
				zap.Time("log_time", logTime),
				zap.Bool("container_in_cache", inCache),
				zap.Int("cache_containers", p.cache.size()),
			}
			if inCache {
				lo := logTime.Add(-p.cfg.Cache.TTL).Add(-p.cfg.MatchTimestampSkew)
				hi := logTime.Add(p.cfg.MatchTimestampSkew)
				fields = append(fields,
					zap.Time("window_lo", lo),
					zap.Time("window_hi", hi),
					zap.Strings("cached_span_starts", p.cache.spanStartTimes(normID)),
				)
			}
			p.logger.Warn("first cache miss for container", fields...)
			p.warnedContainers[normID] = struct{}{}
		}
	}
}

// extractContainerID iterates top-level attribute Maps and navigates the
// configured subpath. Covers process_exec, process_exit, process_kprobe,
// and any future Tetragon event types without per-type configuration.
func (p *logsProcessor) extractContainerID(rec plog.LogRecord) string {
	subpath := splitDotPath(p.cfg.TetragonContainerIDSubpath)
	var found string
	rec.Attributes().Range(func(_ string, v pcommon.Value) bool {
		if v.Type() != pcommon.ValueTypeMap {
			return true
		}
		if id := navigateMap(v.Map(), subpath); id != "" {
			found = id
			return false
		}
		return true
	})
	return found
}

func navigateMap(m pcommon.Map, path []string) string {
	if len(path) == 0 {
		return ""
	}
	v, ok := m.Get(path[0])
	if !ok {
		return ""
	}
	if len(path) == 1 {
		if v.Type() == pcommon.ValueTypeStr {
			return v.Str()
		}
		return ""
	}
	if v.Type() != pcommon.ValueTypeMap {
		return ""
	}
	return navigateMap(v.Map(), path[1:])
}

func splitDotPath(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func resourceServiceName(r pcommon.Resource) string {
	if v, ok := r.Attributes().Get("service.name"); ok {
		return v.Str()
	}
	return ""
}

func resourceContainerID(r pcommon.Resource, attr string) string {
	if v, ok := r.Attributes().Get(attr); ok {
		return v.Str()
	}
	return ""
}

func countSpans(rs ptrace.ResourceSpans) int {
	n := 0
	for i := range rs.ScopeSpans().Len() {
		n += rs.ScopeSpans().At(i).Spans().Len()
	}
	return n
}

func traceIDToHex(id pcommon.TraceID) string { return hex.EncodeToString(id[:]) }
func spanIDToHex(id pcommon.SpanID) string   { return hex.EncodeToString(id[:]) }

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
