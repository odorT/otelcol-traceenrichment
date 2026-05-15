package traceenrichmentprocessor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/processor/processortest"
)

func TestFactory_DefaultConfig(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	assert.NoError(t, cfg.Validate())
	assert.Equal(t, "container.id", cfg.ContainerIDAttribute)
	assert.Equal(t, "process.pod.container.id", cfg.TetragonContainerIDSubpath)
	assert.Equal(t, []string{"trace_id", "span_id", "service.name"}, cfg.EnrichmentAttributes)
}

func TestFactory_SharedCacheAcrossPipelines(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig()
	set := processortest.NewNopSettings(typ)

	tp, err := f.CreateTraces(context.Background(), set, cfg, &consumertest.TracesSink{})
	require.NoError(t, err)

	lp, err := f.CreateLogs(context.Background(), set, cfg, &consumertest.LogsSink{})
	require.NoError(t, err)

	// Both processors must share the same cache instance (same config ID)
	traceP := tp.(*tracesProcessor)
	logsP := lp.(*logsProcessor)
	assert.Same(t, traceP.cache, logsP.cache)
}

func TestFactory_SeparateCacheForDifferentIDs(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig()

	set1 := processortest.NewNopSettings(typ)
	set1.ID = component.NewIDWithName(typ, "inst1")

	set2 := processortest.NewNopSettings(typ)
	set2.ID = component.NewIDWithName(typ, "inst2")

	tp1, err := f.CreateTraces(context.Background(), set1, cfg, &consumertest.TracesSink{})
	require.NoError(t, err)
	tp2, err := f.CreateTraces(context.Background(), set2, cfg, &consumertest.TracesSink{})
	require.NoError(t, err)

	assert.NotSame(t, tp1.(*tracesProcessor).cache, tp2.(*tracesProcessor).cache)
}

func TestFactory_ConfigValidation(t *testing.T) {
	f := NewFactory()
	bad := f.CreateDefaultConfig().(*Config)
	bad.ContainerIDAttribute = ""
	assert.Error(t, bad.Validate())
}
