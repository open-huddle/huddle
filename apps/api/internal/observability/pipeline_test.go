package observability_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/open-huddle/huddle/apps/api/internal/observability"
)

func TestPipelineMetrics_NilReceiverIsSafe(t *testing.T) {
	var p *observability.PipelineMetrics // nil

	require.NotPanics(t, func() {
		p.RecordSendToSubscribe(context.Background(), "created", time.Second)

		reg, err := p.RegisterOutboxDepth(nil) // querier irrelevant on nil receiver
		require.NoError(t, err)
		require.NoError(t, reg.Unregister())
	})
}

func TestPipelineMetrics_RecordSendToSubscribeWritesHistogram(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)

	p, err := observability.NewPipelineMetrics()
	require.NoError(t, err)

	p.RecordSendToSubscribe(context.Background(), "created", 50*time.Millisecond)
	p.RecordSendToSubscribe(context.Background(), "edited", 250*time.Millisecond)
	p.RecordSendToSubscribe(context.Background(), "created", 1*time.Second)

	got := collect(t, reader)
	require.Equal(t, uint64(2),
		histogramCount(t, got, "huddle.pipeline.send_to_subscribe_seconds",
			map[string]string{"message.kind": "created"}))
	require.Equal(t, uint64(1),
		histogramCount(t, got, "huddle.pipeline.send_to_subscribe_seconds",
			map[string]string{"message.kind": "edited"}))
}

func TestPipelineMetrics_RecordSendToSubscribeDropsOutOfRange(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)

	p, err := observability.NewPipelineMetrics()
	require.NoError(t, err)

	// Negative duration (clock skew) and >24h (replay) are dropped — they
	// would skew percentiles and aren't real production signals.
	p.RecordSendToSubscribe(context.Background(), "created", -10*time.Millisecond)
	p.RecordSendToSubscribe(context.Background(), "created", 48*time.Hour)
	p.RecordSendToSubscribe(context.Background(), "created", 100*time.Millisecond)

	got := collect(t, reader)
	require.Equal(t, uint64(1),
		histogramCount(t, got, "huddle.pipeline.send_to_subscribe_seconds",
			map[string]string{"message.kind": "created"}))
}

type stubQuerier struct {
	depth observability.OutboxDepth
	err   error
	calls int
}

func (s *stubQuerier) OutboxDepth(_ context.Context) (observability.OutboxDepth, error) {
	s.calls++
	return s.depth, s.err
}

func TestPipelineMetrics_OutboxDepthEmitsAllFourConsumers(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)

	p, err := observability.NewPipelineMetrics()
	require.NoError(t, err)

	q := &stubQuerier{
		depth: observability.OutboxDepth{
			Unpublished: 7,
			Unindexed:   3,
			Unnotified:  2,
			Unaudited:   5,
		},
	}
	reg, err := p.RegisterOutboxDepth(q)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Unregister() })

	got := collect(t, reader)
	require.Greater(t, q.calls, 0, "callback should fire on Collect")

	require.Equal(t, int64(7), gaugeValue(t, got, "huddle.outbox.depth", map[string]string{"consumer": "publisher"}))
	require.Equal(t, int64(3), gaugeValue(t, got, "huddle.outbox.depth", map[string]string{"consumer": "indexer"}))
	require.Equal(t, int64(2), gaugeValue(t, got, "huddle.outbox.depth", map[string]string{"consumer": "notifications"}))
	require.Equal(t, int64(5), gaugeValue(t, got, "huddle.outbox.depth", map[string]string{"consumer": "audit"}))
}

func TestPipelineMetrics_OutboxDepthSwallowsQuerierError(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)

	p, err := observability.NewPipelineMetrics()
	require.NoError(t, err)

	q := &stubQuerier{err: errors.New("db hiccup")}
	reg, err := p.RegisterOutboxDepth(q)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Unregister() })

	// Collect should not error even though the querier returned one — the
	// callback's contract is "best-effort, retry next scrape".
	got := collect(t, reader)
	require.Greater(t, q.calls, 0)
	// No data points emitted on error — the gauge stays at "no data".
	_ = got // explicit: we don't assert any specific shape here
}

func TestPipelineMetrics_RegisterOutboxDepthRejectsNilQuerier(t *testing.T) {
	mp := metric.NewMeterProvider()
	otel.SetMeterProvider(mp)

	p, err := observability.NewPipelineMetrics()
	require.NoError(t, err)

	_, err = p.RegisterOutboxDepth(nil)
	require.Error(t, err)
}

// gaugeValue returns the int64 value for an observable gauge data point
// matching the metric name and attribute set.
func gaugeValue(t *testing.T, rm metricdata.ResourceMetrics, name string, attrs map[string]string) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				continue
			}
			for _, dp := range g.DataPoints {
				if attributesMatchPipeline(dp.Attributes, attrs) {
					return dp.Value
				}
			}
		}
	}
	t.Fatalf("gauge %q with attrs %v not found", name, attrs)
	return 0
}

func attributesMatchPipeline(set attribute.Set, want map[string]string) bool {
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
