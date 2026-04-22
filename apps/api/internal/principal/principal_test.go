package principal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/open-huddle/huddle/apps/api/ent/user"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
)

// newResolver pairs a fresh per-test SQLite client with the Resolver under
// test. Each subtest gets its own isolated DB via testutil.NewClient.
func newResolver(t *testing.T) (*principal.Resolver, context.Context) {
	t.Helper()
	client := testutil.NewClient(t)
	return principal.NewResolver(client), context.Background()
}

func ctxWithClaims(ctx context.Context, c auth.Claims) context.Context {
	return auth.WithClaims(ctx, c)
}

func TestResolve_FirstCall_UpsertsNewUser(t *testing.T) {
	t.Parallel()
	r, ctx := newResolver(t)
	ctx = ctxWithClaims(ctx, auth.Claims{
		Subject:       "alice-subject",
		Email:         "alice@example.test",
		EmailVerified: true,
		Name:          "Alice Anderson",
	})

	u, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.Subject != "alice-subject" {
		t.Errorf("subject: want %q got %q", "alice-subject", u.Subject)
	}
	if u.Email != "alice@example.test" {
		t.Errorf("email: want %q got %q", "alice@example.test", u.Email)
	}
	if u.DisplayName != "Alice Anderson" {
		t.Errorf("display_name: want %q got %q", "Alice Anderson", u.DisplayName)
	}
}

func TestResolve_SecondCall_ReturnsSameRow(t *testing.T) {
	t.Parallel()
	r, ctx := newResolver(t)
	ctx = ctxWithClaims(ctx, auth.Claims{Subject: "alice", Email: "a@example.test", Name: "Alice"})

	first, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("second call must return the same user row (id %s) but got %s", first.ID, second.ID)
	}
}

// When a token arrives with the same subject but refreshed email / name
// (IDP-side rename, email change), the resolver must propagate those
// fields so downstream queries see current data. The `sub` claim is the
// stable identity — email and display_name update in place.
func TestResolve_SecondCall_RefreshesChangedFields(t *testing.T) {
	t.Parallel()
	r, ctx := newResolver(t)
	initial := ctxWithClaims(ctx, auth.Claims{Subject: "bob", Email: "bob@old.test", Name: "Bob"})
	if _, err := r.Resolve(initial); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	updated := ctxWithClaims(ctx, auth.Claims{Subject: "bob", Email: "bob@new.test", Name: "Robert"})
	u, err := r.Resolve(updated)
	if err != nil {
		t.Fatalf("refresh Resolve: %v", err)
	}
	if u.Email != "bob@new.test" {
		t.Errorf("email should have refreshed to bob@new.test, got %q", u.Email)
	}
	if u.DisplayName != "Robert" {
		t.Errorf("display_name should have refreshed to Robert, got %q", u.DisplayName)
	}
}

// DisplayName falls back to preferred_username when Keycloak doesn't emit
// `name` (depends on realm-level mapper config). The resolver uses
// Claims.DisplayName() which owns that fallback.
func TestResolve_FallsBackToPreferredUsernameForDisplayName(t *testing.T) {
	t.Parallel()
	r, ctx := newResolver(t)
	ctx = ctxWithClaims(ctx, auth.Claims{
		Subject:           "carla",
		Email:             "carla@example.test",
		PreferredUsername: "carla",
		// Name intentionally empty — simulates a realm without the `name` mapper.
	})

	u, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.DisplayName != "carla" {
		t.Errorf("display_name should fall back to preferred_username, got %q", u.DisplayName)
	}
}

// Reaching Resolve without the auth interceptor having run (i.e. no claims
// on the context) is a programming error — the handler must not be mounted
// on an unauthenticated route. Test asserts the resolver surfaces
// auth.ErrNoClaims so callers can map it to Unauthenticated.
func TestResolve_NoClaims_ReturnsErrNoClaims(t *testing.T) {
	t.Parallel()
	r, _ := newResolver(t)

	_, err := r.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, auth.ErrNoClaims) {
		t.Errorf("want errors.Is(err, auth.ErrNoClaims), got: %v", err)
	}
}

// Sanity: after Resolve lands, the user is actually discoverable by its
// subject index (the UNIQUE we rely on in policy/rbac.go's lookupRole).
func TestResolve_UserIsFindableBySubject(t *testing.T) {
	t.Parallel()
	client := testutil.NewClient(t)
	r := principal.NewResolver(client)
	ctx := ctxWithClaims(context.Background(), auth.Claims{
		Subject: "dave", Email: "d@example.test", Name: "Dave",
	})

	if _, err := r.Resolve(ctx); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, err := client.User.Query().Where(user.SubjectEQ("dave")).Only(ctx)
	if err != nil {
		t.Fatalf("lookup by subject: %v", err)
	}
	if got.Subject != "dave" {
		t.Errorf("subject: want dave got %q", got.Subject)
	}
}
