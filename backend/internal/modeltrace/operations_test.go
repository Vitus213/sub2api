package modeltrace

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestModelTraceOperationalSignals(t *testing.T) {
	t.Run("successful export", func(t *testing.T) {
		stats := &exportStats{source: ConfigSourceRuntime, version: 7}
		stats.endedSpans.Add(2)
		exporter := failOpenExporter{delegate: benchmarkDiscardExporter{}, stats: stats}
		require.NoError(t, exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{nil, nil}))
		require.Equal(t, exportStatsSnapshot{Ended: 2, Attempted: 2, Exported: 2}, stats.snapshot())
	})

	t.Run("failed export", func(t *testing.T) {
		stats := &exportStats{source: ConfigSourceRuntime, version: 8}
		stats.endedSpans.Add(3)
		exporter := failOpenExporter{delegate: errorSpanExporter{}, stats: stats}
		require.Error(t, exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{nil, nil}))
		require.Equal(t, exportStatsSnapshot{Ended: 3, Attempted: 2, Failed: 2, PendingOrDropped: 1}, stats.snapshot())
	})

	t.Run("panic is counted without escaping", func(t *testing.T) {
		stats := &exportStats{source: ConfigSourceRuntime, version: 9}
		stats.endedSpans.Add(1)
		exporter := failOpenExporter{delegate: panicSpanExporter{}, stats: stats}
		require.Error(t, exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{nil}))
		require.Equal(t, exportStatsSnapshot{Ended: 1, Attempted: 1, Failed: 1, Panics: 1}, stats.snapshot())
	})
}

func TestModelTraceRolloutDisabledByDefault(t *testing.T) {
	manager, err := NewManager(context.Background(), config.ModelTracingConfig{})
	require.NoError(t, err)
	require.False(t, manager.Enabled())
	snapshot := manager.Acquire()
	require.Equal(t, ConfigSourceDeployment, snapshot.Source())
	snapshot.Release()
	shutdownManager(t, manager)
}

func TestModelTraceRollbackSwitch(t *testing.T) {
	target := newFakeOTLPServer(t)
	manager := newAsyncTestManager(t, target.server.URL)
	require.True(t, manager.Enabled())

	require.NoError(t, manager.ApplySnapshot(context.Background(), ConfigSnapshot{
		Config: config.ModelTracingConfig{
			Enabled: false, PromptMaxBytes: defaultPromptBytes,
			ResponseMaxBytes: defaultResponseBytes, MediaMaxBytes: defaultMediaBytes,
		},
		Source: ConfigSourceRuntime, ConfigVersion: 1,
	}))
	require.False(t, manager.Enabled())
	shutdownManager(t, manager)
}
