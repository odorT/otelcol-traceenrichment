package traceenrichmentprocessor

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

func testCacheConfig() CacheConfig {
	return CacheConfig{
		TTL:             30 * time.Second,
		MaxEntries:      1000,
		CleanupInterval: 5 * time.Second,
	}
}

func makeTraceID(b byte) pcommon.TraceID {
	var id pcommon.TraceID
	id[0] = b
	return id
}

func makeSpanID(b byte) pcommon.SpanID {
	var id pcommon.SpanID
	id[0] = b
	return id
}

// --- normalizeContainerID ---

func TestNormalizeContainerID_ContainerdPrefix(t *testing.T) {
	assert.Equal(t, "abc123", normalizeContainerID("containerd://abc123"))
}

func TestNormalizeContainerID_DockerPrefix(t *testing.T) {
	assert.Equal(t, "abc123", normalizeContainerID("docker://abc123"))
}

func TestNormalizeContainerID_CrioPrefix(t *testing.T) {
	assert.Equal(t, "abc123", normalizeContainerID("cri-o://abc123"))
}

func TestNormalizeContainerID_Bare(t *testing.T) {
	assert.Equal(t, "abc123", normalizeContainerID("abc123"))
}

// --- put / lookup ---

func TestCache_HitWithinWindow(t *testing.T) {
	c := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	now := time.Now()

	c.put("ctr1", spanEntry{
		TraceID:     makeTraceID(1),
		SpanID:      makeSpanID(1),
		ServiceName: "svc-a",
		StartTime:   now,
		SeenAt:      now,
	})

	entry, ok := c.lookup("ctr1", now.Add(100*time.Millisecond))
	require.True(t, ok)
	assert.Equal(t, makeTraceID(1), entry.TraceID)
	assert.Equal(t, "svc-a", entry.ServiceName)
}

func TestCache_MissUnknownContainer(t *testing.T) {
	c := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	_, ok := c.lookup("nonexistent", time.Now())
	assert.False(t, ok)
}

func TestCache_MissOutsideWindow(t *testing.T) {
	c := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	now := time.Now()

	c.put("ctr1", spanEntry{
		StartTime: now,
		SeenAt:    now,
	})

	// Log timestamp 60s after span start — outside TTL+skew window
	_, ok := c.lookup("ctr1", now.Add(60*time.Second))
	assert.False(t, ok)
}

func TestCache_MostRecentSpanSelected(t *testing.T) {
	c := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	now := time.Now()

	c.put("ctr1", spanEntry{TraceID: makeTraceID(1), StartTime: now.Add(-2 * time.Second), SeenAt: now})
	c.put("ctr1", spanEntry{TraceID: makeTraceID(2), StartTime: now.Add(-1 * time.Second), SeenAt: now})

	entry, ok := c.lookup("ctr1", now)
	require.True(t, ok)
	// Most recently started span wins
	assert.Equal(t, makeTraceID(2), entry.TraceID)
}

func TestCache_RingBufferEvictsOldest(t *testing.T) {
	c := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	now := time.Now()

	// Fill ring buffer to capacity + 1
	for i := range SpansPerContainer + 1 {
		c.put("ctr1", spanEntry{
			TraceID:   makeTraceID(byte(i)),
			StartTime: now.Add(time.Duration(i) * time.Millisecond),
			SeenAt:    now,
		})
	}

	ce := c.entries["ctr1"]
	assert.Equal(t, SpansPerContainer, len(ce.Spans))
	// Oldest (index 0, TraceID byte 0) should have been dropped
	assert.Equal(t, makeTraceID(1), ce.Spans[0].TraceID)
}

// --- eviction ---

func TestCache_TTLEviction(t *testing.T) {
	cfg := CacheConfig{TTL: 10 * time.Millisecond, MaxEntries: 1000, CleanupInterval: time.Hour}
	c := newSpanCache(cfg, 0)

	c.put("ctr1", spanEntry{SeenAt: time.Now().Add(-100 * time.Millisecond)})
	assert.Equal(t, 1, c.size())

	ttl, max := c.evict()
	assert.Equal(t, 1, ttl)
	assert.Equal(t, 0, max)
	assert.Equal(t, 0, c.size())
}

func TestCache_MaxEntriesEviction(t *testing.T) {
	cfg := CacheConfig{TTL: 30 * time.Second, MaxEntries: 2, CleanupInterval: time.Hour}
	c := newSpanCache(cfg, 0)

	now := time.Now()
	c.put("ctr1", spanEntry{SeenAt: now})
	c.put("ctr2", spanEntry{SeenAt: now.Add(time.Millisecond)})
	c.put("ctr3", spanEntry{SeenAt: now.Add(2 * time.Millisecond)})
	assert.Equal(t, 3, c.size())

	_, max := c.evict()
	assert.Equal(t, 1, max)
	assert.Equal(t, 2, c.size())
	// Oldest (ctr1) should be gone
	_, ok := c.entries["ctr1"]
	assert.False(t, ok)
}

// --- concurrency ---

func TestCache_ConcurrentPutLookup(t *testing.T) {
	c := newSpanCache(testCacheConfig(), 500*time.Millisecond)
	now := time.Now()
	var wg sync.WaitGroup

	for i := range 20 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.put("ctr1", spanEntry{
				TraceID:   makeTraceID(byte(i)),
				StartTime: now,
				SeenAt:    now,
			})
		}(i)
		go func() {
			defer wg.Done()
			c.lookup("ctr1", now)
		}()
	}
	wg.Wait()
}
