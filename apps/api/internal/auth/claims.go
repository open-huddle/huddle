package auth

import (
	"context"
	"errors"
)

// Claims carries the OIDC claims we act on after a successful token verify.
// It is deliberately small — add fields only when a handler actually needs
// them, since every field here is a contract with the IDP.
type Claims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	// Name falls back to preferred_username when the IDP (Keycloak) emits
	// `name` only after a mapper is configured. We read both.
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// DisplayName returns the best human-readable name from the claims.
func (c Claims) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.PreferredUsername
}

type ctxKey struct{}

// WithClaims returns ctx with claims attached. Interceptors call this after
// verifying a token; handlers read back with ClaimsFrom.
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// ClaimsFrom returns the claims attached to ctx by the auth interceptor.
// The bool is false only on a route that was not wrapped by the interceptor,
// which is a programming error — callers should treat that as unauthenticated.
func ClaimsFrom(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}

// ErrNoClaims is returned to handlers that expect claims but were reached via
// an unprotected route. Use connect.NewError(connect.CodeUnauthenticated, ...)
// to convert.
var ErrNoClaims = errors.New("auth: no claims on context")
