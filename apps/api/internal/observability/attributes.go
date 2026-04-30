package observability

import (
	"context"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// AttributeReader extracts span attributes from a request context.
// Typically reads claims/principal info that an upstream interceptor
// stashed on the context. Kept as a function seam so the observability
// package has no dependency on the auth or principal packages — the
// caller in server.go closes over those imports.
type AttributeReader func(ctx context.Context) []attribute.KeyValue

// AttributeInterceptor returns a Connect interceptor that decorates the
// active span (created by otelconnect upstream in the chain) with the
// reader's attributes. Place AFTER the auth interceptor so the reader
// sees an authenticated context.
//
// A non-recording span (observability disabled, or the route isn't
// otel-wrapped) drops attributes silently. A nil or zero-length reader
// result is a no-op.
func AttributeInterceptor(reader AttributeReader) connect.Interceptor {
	return &attributeInterceptor{reader: reader}
}

type attributeInterceptor struct {
	reader AttributeReader
}

func (a *attributeInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		a.enrich(ctx)
		return next(ctx, req)
	}
}

// WrapStreamingClient is a no-op: this process is a server, not an
// outbound streaming client.
func (a *attributeInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *attributeInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		a.enrich(ctx)
		return next(ctx, conn)
	}
}

func (a *attributeInterceptor) enrich(ctx context.Context) {
	if a.reader == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	if attrs := a.reader(ctx); len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}
