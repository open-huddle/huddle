package observability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/open-huddle/huddle/apps/api/internal/observability"
)

func TestWorkerInstr_NilReceiverIsSafe(t *testing.T) {
	var instr *observability.WorkerInstr // nil

	// All three methods should be safe and behave as if no
	// instrumentation was wired.
	require.NotPanics(t, func() {
		err := instr.Tick(context.Background(), func(_ context.Context) error { return nil })
		require.NoError(t, err)

		_, end := instr.StartRow(context.Background(), attribute.String("k", "v"))
		end(nil)
		end(errors.New("boom"))

		instr.AddRows(context.Background(), 5)
	})
}

func TestWorkerInstr_TickRecordsDurationAndErrors(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)

	metrics, err := observability.NewWorkerMetrics()
	require.NoError(t, err)
	instr := observability.NewWorkerInstr("widget", metrics)

	// Successful tick — duration recorded, no error.
	require.NoError(t, instr.Tick(context.Background(), func(_ context.Context) error { return nil }))

	// Failing tick — duration recorded again, errors{scope=tick} = 1.
	bad := errors.New("boom")
	require.ErrorIs(t,
		instr.Tick(context.Background(), func(_ context.Context) error { return bad }),
		bad)

	got := collect(t, reader)

	require.Equal(t, int64(1), counterValue(t, got, "huddle.worker.errors", map[string]string{"worker": "widget", "scope": "tick"}))
	require.Equal(t, uint64(2), histogramCount(t, got, "huddle.worker.tick_duration_seconds", map[string]string{"worker": "widget"}))
}

func TestWorkerInstr_StartRowRecordsCountersAndAttributes(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)

	metrics, err := observability.NewWorkerMetrics()
	require.NoError(t, err)
	instr := observability.NewWorkerInstr("widget", metrics)

	// Two successful rows, one failing row.
	for i := 0; i < 2; i++ {
		_, end := instr.StartRow(context.Background())
		end(nil)
	}
	_, end := instr.StartRow(context.Background())
	end(errors.New("row-bad"))

	got := collect(t, reader)

	require.Equal(t, int64(3), counterValue(t, got, "huddle.worker.rows_processed", map[string]string{"worker": "widget"}))
	require.Equal(t, int64(1), counterValue(t, got, "huddle.worker.errors", map[string]string{"worker": "widget", "scope": "row"}))
}

func TestWorkerInstr_AddRowsBumpsCounter(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)

	metrics, err := observability.NewWorkerMetrics()
	require.NoError(t, err)
	instr := observability.NewWorkerInstr("widget", metrics)

	instr.AddRows(context.Background(), 7)
	instr.AddRows(context.Background(), 0)  // no-op
	instr.AddRows(context.Background(), -3) // no-op

	got := collect(t, reader)
	require.Equal(t, int64(7), counterValue(t, got, "huddle.worker.rows_processed", map[string]string{"worker": "widget"}))
}

// --- helpers ---

func collect(t *testing.T, r metric.Reader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, r.Collect(context.Background(), &rm))
	return rm
}

// counterValue returns the int64 sum of the data point matching the
// metric name and attribute set. Fails the test if no match is found.
func counterValue(t *testing.T, rm metricdata.ResourceMetrics, name string, attrs map[string]string) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if attributesMatch(dp.Attributes, attrs) {
					return dp.Value
				}
			}
		}
	}
	t.Fatalf("counter %q with attrs %v not found", name, attrs)
	return 0
}

// histogramCount returns the count (number of recorded observations)
// for the histogram data point matching attributes.
func histogramCount(t *testing.T, rm metricdata.ResourceMetrics, name string, attrs map[string]string) uint64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range h.DataPoints {
				if attributesMatch(dp.Attributes, attrs) {
					return dp.Count
				}
			}
		}
	}
	t.Fatalf("histogram %q with attrs %v not found", name, attrs)
	return 0
}

func attributesMatch(set attribute.Set, want map[string]string) bool {
	if set.Len() != len(want) {
		return false
	}
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.AsString() != v {
			return false
		}
	}
	return true
}
