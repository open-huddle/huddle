package auth

import (
	"context"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Verifier wraps an *oidc.IDTokenVerifier with the audience this API expects.
// Keeping the wrapper narrow lets handlers and interceptors depend on a small
// surface instead of the full go-oidc API.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewVerifier dials the OIDC discovery endpoint at issuerURL, fetches the
// JWKS, and returns a Verifier pinned to the given audience. The returned
// Verifier keeps the JWKS fresh in the background, so this function should
// be called once at startup.
func NewVerifier(ctx context.Context, issuerURL, audience string) (*Verifier, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery %q: %w", issuerURL, err)
	}
	v := provider.Verifier(&oidc.Config{
		ClientID: audience, // go-oidc calls the audience "ClientID" — here it is the aud claim, not the OAuth client.
	})
	return &Verifier{verifier: v}, nil
}

// Verify parses and verifies a raw JWT and extracts the subset of claims the
// API acts on.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (Claims, error) {
	tok, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	if err := tok.Claims(&c); err != nil {
		return Claims{}, fmt.Errorf("parse claims: %w", err)
	}
	if c.Subject == "" {
		// The IDP should never issue an empty subject, but refuse to act on one
		// defensively — the User.subject index requires NotEmpty.
		return Claims{}, fmt.Errorf("token missing subject claim")
	}
	return c, nil
}
