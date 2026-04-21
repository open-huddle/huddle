package auth

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
)

// NewInterceptor returns a unary Connect interceptor that verifies a bearer
// token on every call and stashes the resulting Claims on the context. It is
// applied per-service so public RPCs (health) are not forced through token
// verification.
func NewInterceptor(v *Verifier) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			raw, err := bearerFromHeader(req.Header().Get("Authorization"))
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			claims, err := v.Verify(ctx, raw)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated, err)
			}
			return next(WithClaims(ctx, claims), req)
		}
	})
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
