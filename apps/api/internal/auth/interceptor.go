package auth

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
)

// NewInterceptor returns a Connect interceptor that verifies a bearer token
// on every call (unary and streaming) and stashes the resulting Claims on
// the context. It is applied per-service so public RPCs (health) are not
// forced through token verification.
//
// Implemented as a full Interceptor rather than UnaryInterceptorFunc so
// streaming handlers (e.g. MessageService.Subscribe) are wrapped too —
// otherwise streams would skip authentication entirely.
//
// Takes TokenVerifier (interface) rather than *Verifier so tests can drop
// in a fake without standing up a real OIDC provider.
func NewInterceptor(v TokenVerifier) connect.Interceptor {
	return &interceptor{v: v}
}

type interceptor struct {
	v TokenVerifier
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		raw, err := bearerFromHeader(req.Header().Get("Authorization"))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
		claims, err := i.v.Verify(ctx, raw)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
		return next(WithClaims(ctx, claims), req)
	}
}

// WrapStreamingClient is a no-op: this process is a server, not an outbound
// streaming client. Returning next unmodified satisfies the interface.
func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		raw, err := bearerFromHeader(conn.RequestHeader().Get("Authorization"))
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, err)
		}
		claims, err := i.v.Verify(ctx, raw)
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, err)
		}
		return next(WithClaims(ctx, claims), conn)
	}
}

func bearerFromHeader(h string) (string, error) {
	if h == "" {
		return "", errors.New("missing Authorization header")
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", errors.New("authorization header is not a Bearer token")
	}
	return strings.TrimSpace(h[len(prefix):]), nil
}
