package observability_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/open-huddle/huddle/apps/api/internal/observability"
)

type ctxKey struct{}

// Minimal request type — connect.AnyRequest's only requirement we use
// is being non-nil; the interceptor never reads the payload.
type fakeReq struct {
	connect.Request[struct{}]
}

func TestAttributeInterceptor_AddsAttrsToActiveSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)

	called := false
	interceptor := observability.AttributeInterceptor(func(ctx context.Context) []attribute.KeyValue {
		called = true
		v, _ := ctx.Value(ctxKey{}).(string)
		return []attribute.KeyValue{
			attribute.String("huddle.user.subject", v),
		}
	})

	tracer := tp.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "rpc")
	ctx = context.WithValue(ctx, ctxKey{}, "subj-42")

	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})
	_, err := interceptor.WrapUnary(next)(ctx, &fakeReq{})
	require.NoError(t, err)
	span.End()

	require.True(t, called, "reader should have been called")
	spans := rec.Ended()
	require.Len(t, spans, 1)
	got, ok := spans[0].Attributes(), false
	for _, kv := range got {
		if string(kv.Key) == "huddle.user.subject" && kv.Value.AsString() == "subj-42" {
			ok = true
		}
	}
	require.True(t, ok, "expected huddle.user.subject=subj-42 on span; got %v", got)
}

func TestAttributeInterceptor_NonRecordingSpanIsNoOp(t *testing.T) {
	// Default global tracer provider is no-op. Reader is still called
	// (the interceptor doesn't know in advance the span is non-recording),
	// but no SetAttributes call lands anywhere.
	otel.SetTracerProvider(otel.GetTracerProvider()) // restore default no-op

	called := 0
	interceptor := observability.AttributeInterceptor(func(_ context.Context) []attribute.KeyValue {
		called++
		return []attribute.KeyValue{attribute.String("k", "v")}
	})

	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})
	_, _ = interceptor.WrapUnary(next)(context.Background(), &fakeReq{})

	// With a non-recording span the interceptor short-circuits BEFORE
	// calling the reader — no attribute work to do.
	require.Equal(t, 0, called)
}

func TestAttributeInterceptor_NilReaderIsSafe(t *testing.T) {
	interceptor := observability.AttributeInterceptor(nil)

	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	})
	_, err := interceptor.WrapUnary(next)(context.Background(), &fakeReq{})
	require.NoError(t, err)
}

// Sanity: the interceptor isn't supposed to swallow next's error.
func TestAttributeInterceptor_PassesThroughNextError(t *testing.T) {
	interceptor := observability.AttributeInterceptor(nil)
	want := errors.New("downstream")

	next := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, want
	})
	_, err := interceptor.WrapUnary(next)(context.Background(), &fakeReq{})
	require.ErrorIs(t, err, want)
}
