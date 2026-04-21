// Package testutil wires up the in-test database and common fixtures. It uses
// modernc.org/sqlite so tests stay cgo-free and run on the same ent schema
// that production uses — ent's dialect abstraction handles the Postgres →
// SQLite differences transparently for everything except pgconn-specific
// error codes (see services/organization/errors.go isUniqueViolation, which
// is exercised by a separate Postgres integration test).
package testutil

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	// modernc.org/sqlite registers the cgo-free "sqlite" driver with database/sql.
	_ "modernc.org/sqlite"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/enttest"
	entmembership "github.com/open-huddle/huddle/apps/api/ent/membership"

	// ent/runtime wires schema hooks; required for any client using this package.
	_ "github.com/open-huddle/huddle/apps/api/ent/runtime"
)

// NewClient returns an ent.Client backed by a fresh per-test SQLite database.
// The file lives in t.TempDir() and is cleaned up automatically. Foreign keys
// are enabled; migrations are applied via enttest before returning.
func NewClient(t *testing.T) *ent.Client {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db") + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// ent expects the "sqlite3" dialect name regardless of the registered
	// driver name; wrap the *sql.DB explicitly so modernc.org/sqlite works.
	drv := entsql.OpenDB("sqlite3", db)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// MakeUser inserts a user with the given OIDC subject and predictable email.
// Returns the created row.
func MakeUser(ctx context.Context, t *testing.T, client *ent.Client, subject string) *ent.User {
	t.Helper()
	u, err := client.User.Create().
		SetSubject(subject).
		SetEmail(subject + "@example.test").
		SetDisplayName(subject).
		Save(ctx)
	if err != nil {
		t.Fatalf("make user %q: %v", subject, err)
	}
	return u
}

// MakeOrg inserts an organization with a deterministic slug.
func MakeOrg(ctx context.Context, t *testing.T, client *ent.Client, slug string) *ent.Organization {
	t.Helper()
	o, err := client.Organization.Create().
		SetName(slug).
		SetSlug(slug).
		Save(ctx)
	if err != nil {
		t.Fatalf("make org %q: %v", slug, err)
	}
	return o
}

// MakeMember inserts a membership tying u to o with the given role.
func MakeMember(ctx context.Context, t *testing.T, client *ent.Client, u *ent.User, o *ent.Organization, role entmembership.Role) *ent.Membership {
	t.Helper()
	m, err := client.Membership.Create().
		SetUser(u).
		SetOrganization(o).
		SetRole(role).
		Save(ctx)
	if err != nil {
		t.Fatalf("make membership: %v", err)
	}
	return m
}

// MustParseUUID fails the test on parse error — for when the caller knows the
// string is a valid UUID (e.g. it came from a proto response).
func MustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}
