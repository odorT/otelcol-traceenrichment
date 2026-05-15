package traceenrichmentprocessor

import (
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

const SpansPerContainer = 64

type spanEntry struct {
	TraceID     pcommon.TraceID
	SpanID      pcommon.SpanID
	ServiceName string
	StartTime   time.Time
	EndTime     time.Time // stored per FR1; matching uses TTL window, not EndTime directly
	SeenAt      time.Time
}

type containerEntry struct {
	Spans     []spanEntry
	LastTouch time.Time
}

type spanCache struct {
	mu      sync.RWMutex
	entries map[string]*containerEntry
	cfg     CacheConfig
	skew    time.Duration
}

func newSpanCache(cfg CacheConfig, skew time.Duration) *spanCache {
	return &spanCache{
		entries: make(map[string]*containerEntry),
		cfg:     cfg,
		skew:    skew,
	}
}

// normalizeContainerID strips runtime scheme prefixes so that
// containerd://<id>, docker://<id>, cri-o://<id>, and bare <id>
// all resolve to the same cache key.
func normalizeContainerID(id string) string {
	for _, prefix := range []string{"containerd://", "docker://", "cri-o://"} {
		if strings.HasPrefix(id, prefix) {
			return id[len(prefix):]
		}
	}
	return id
}

// put inserts a span entry into the cache for the given container.
// If the ring buffer is full, the oldest entry is dropped.
func (c *spanCache) put(containerID string, entry spanEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ce, ok := c.entries[containerID]
	if !ok {
		ce = &containerEntry{}
		c.entries[containerID] = ce
	}
	ce.LastTouch = time.Now()
	if len(ce.Spans) >= SpansPerContainer {
		ce.Spans = ce.Spans[1:]
	}
	ce.Spans = append(ce.Spans, entry)
}

// lookup finds the most recently started span for a container whose active
// window overlaps the given log timestamp. Returns false on miss.
//
// Match window: span.StartTime ∈ [logTime - TTL - skew, logTime + skew]
// The TTL extends backwards to cover events that fire after a short-lived span ends.
func (c *spanCache) lookup(containerID string, logTime time.Time) (spanEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ce, ok := c.entries[containerID]
	if !ok {
		return spanEntry{}, false
	}

	now := time.Now()
	// Match window anchored on log timestamp per spec:
	// span.StartTime ∈ [logTime - TTL - skew, logTime + skew]
	lo := logTime.Add(-c.cfg.TTL).Add(-c.skew)
	hi := logTime.Add(c.skew)

	var best spanEntry
	found := false

	for _, s := range ce.Spans {
		if s.SeenAt.Add(c.cfg.TTL).Before(now) {
			continue // lazily skip expired entries
		}
		if !s.StartTime.Before(lo) && !s.StartTime.After(hi) {
			if !found || s.StartTime.After(best.StartTime) {
				best = s
				found = true
			}
		}
	}
	return best, found
}

// size returns the number of container entries currently in the cache.
func (c *spanCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// contains reports whether the container ID has any entry in the cache,
// regardless of whether a span in it would match a given time window.
func (c *spanCache) contains(containerID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.entries[containerID]
	return ok
}

// spanStartTimes returns the StartTime of every cached span for the container,
// formatted as RFC3339Nano. Used only for debug logging.
func (c *spanCache) spanStartTimes(containerID string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ce, ok := c.entries[containerID]
	if !ok {
		return nil
	}
	out := make([]string, len(ce.Spans))
	for i, s := range ce.Spans {
		out[i] = s.StartTime.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// evict removes expired span entries and enforces MaxEntries.
// Returns the number of evictions by cause.
func (c *spanCache) evict() (ttlEvictions, maxEvictions int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	for key, ce := range c.entries {
		live := ce.Spans[:0]
		for _, s := range ce.Spans {
			if s.SeenAt.Add(c.cfg.TTL).After(now) {
				live = append(live, s)
			} else {
				ttlEvictions++
			}
		}
		if len(live) == 0 {
			delete(c.entries, key)
		} else {
			ce.Spans = live
		}
	}

	for len(c.entries) > c.cfg.MaxEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, ce := range c.entries {
			if oldestKey == "" || ce.LastTouch.Before(oldestTime) {
				oldestKey = k
				oldestTime = ce.LastTouch
			}
		}
		delete(c.entries, oldestKey)
		maxEvictions++
	}
	return
}
