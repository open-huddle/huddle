//go:build integration

// Integration tests for OrganizationService that require Postgres. The
// SQLite-based unit suite in organization_test.go covers everything except
// the pgx-specific error-code mapping exercised here; that mapping needs
// the real PgError (SQLSTATE 23505) to travel up the stack before
// isUniqueViolation can recognise it.
//
// Run with `make test-integration` (Docker required).
package organization_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"

	"github.com/open-huddle/huddle/apps/api/internal/policy"
	"github.com/open-huddle/huddle/apps/api/internal/principal"
	"github.com/open-huddle/huddle/apps/api/internal/services/organization"
	"github.com/open-huddle/huddle/apps/api/internal/testutil"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// newPostgresFixture mirrors newFixture(t) from the unit suite but uses the
// Postgres-backed client. Kept local to this file so the unit-suite fixture
// stays ignorant of the integration mode.
func newPostgresFixture(t *testing.T) *fixture {
	t.Helper()
	client := testutil.NewPostgresClient(t)
	resolver := principal.NewResolver(client)
	engine := policy.NewRBAC(client)
	svc := organization.New(client, resolver, engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &fixture{ctx: context.Background(), client: client, svc: svc}
}

// TestCreate_DuplicateSlug_IsAlreadyExists covers the path the SQLite test
// cannot: ent returns a *pgconn.PgError with code 23505 on a UNIQUE
// violation, isUniqueViolation recognises it, the handler maps it to
// connect.CodeAlreadyExists (not CodeInternal).
//
// The unit test for this same scenario on SQLite falls through to
// CodeInternal because modernc.org/sqlite returns a plain text error
// without a pgx-style code. That's the documented SQLite limitation;
// this test closes the coverage gap.
func TestCreate_DuplicateSlug_IsAlreadyExists(t *testing.T) {
	f := newPostgresFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	req := &huddlev1.CreateRequest{
		Name: "Acme",
		Slug: "acme",
	}

	if _, err := f.svc.Create(ctx, connect.NewRequest(req)); err != nil {
		t.Fatalf("first Create should succeed, got: %v", err)
	}

	// Bob trying to land the same slug (as a new caller) must hit the
	// UNIQUE(slug) constraint and bubble up as AlreadyExists. Use a
	// different subject so the auth resolver upserts a fresh user row —
	// the collision must be on the organization slug, not on the user.
	bobCtx := callerCtx(f.ctx, "bob")
	_, err := f.svc.Create(bobCtx, connect.NewRequest(req))
	if got, want := connectCode(t, err), connect.CodeAlreadyExists; got != want {
		t.Errorf("code: want %v got %v (err=%v)", want, got, err)
	}
}
