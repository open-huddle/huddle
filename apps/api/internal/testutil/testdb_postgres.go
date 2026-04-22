//go:build integration

// Integration-mode test helpers. Compiled only when the `integration` build
// tag is active (`make test-integration` / `go test -tags integration`).
// Keeping this file behind a tag means plain `go test ./...` stays
// cgo-free and container-free; contributors without Docker still get
// useful coverage from the SQLite path in testdb.go.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	// pgx/stdlib registers the "pgx" driver with database/sql — same as
	// internal/database's production path.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-huddle/huddle/apps/api/ent"
	// ent/runtime wires schema hooks; required for any client using this package.
	_ "github.com/open-huddle/huddle/apps/api/ent/runtime"
)

// Shared container per `go test` run. Cheaper than one-container-per-test —
// bringing Postgres up takes seconds; we amortize it across every test in
// the package. Each test gets its own database inside the shared container
// for isolation (see NewPostgresClient).
var (
	pgOnce     sync.Once
	pgURL      string
	pgURLErr   error
	pgCleanup  func()
	pgInitLock sync.Mutex
)

// sharedPostgres starts a Postgres container on first call and returns the
// admin connection URL. Subsequent calls within the same test run reuse it.
// The container terminates when the test process exits — testcontainers-go
// registers a reaper container that cleans up orphaned test containers on
// the host.
func sharedPostgres(t *testing.T) string {
	t.Helper()
	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Match the Postgres major version we run in deploy/compose so
		// integration tests exercise the same server behavior prod will.
		container, err := postgres.Run(ctx,
			"postgres:18.3-alpine",
			postgres.WithDatabase("huddle_test"),
			postgres.WithUsername("huddle"),
			postgres.WithPassword("huddle"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second),
			),
		)
		if err != nil {
			pgURLErr = fmt.Errorf("start postgres container: %w", err)
			return
		}
		pgCleanup = func() {
			_ = container.Terminate(context.Background())
		}
		url, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			pgURLErr = fmt.Errorf("connection string: %w", err)
			return
		}
		pgURL = url
	})
	if pgURLErr != nil {
		t.Fatalf("shared postgres unavailable: %v", pgURLErr)
	}
	return pgURL
}

// NewPostgresClient returns an ent.Client backed by a **fresh database
// inside the shared Postgres container**. The database is created on entry
// and dropped on test cleanup, so tests do not leak state into each other.
// The container itself stays alive across tests for speed.
//
// Schema is applied via ent's Schema.Create — same DDL path production
// uses, covers the annotations (FK cascade, unique nullable, etc.) that
// Atlas migrations emit. Integration tests assert behaviors that SQLite
// cannot (pgx error codes, FOR UPDATE SKIP LOCKED); the schema shape does
// not diverge from production.
func NewPostgresClient(t *testing.T) *ent.Client {
	t.Helper()
	adminURL := sharedPostgres(t)

	dbName := "huddle_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	adminDB, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin sql.DB: %v", err)
	}
	defer adminDB.Close()

	pgInitLock.Lock()
	_, execErr := adminDB.ExecContext(context.Background(),
		fmt.Sprintf(`CREATE DATABASE %s`, dbName))
	pgInitLock.Unlock()
	if execErr != nil {
		t.Fatalf("create test database %q: %v", dbName, execErr)
	}
	t.Cleanup(func() {
		// Open a short-lived admin conn for drop — the per-test client has
		// already been closed by its own t.Cleanup by the time this runs.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		db, err := sql.Open("pgx", adminURL)
		if err != nil {
			t.Logf("cleanup: open admin: %v", err)
			return
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName)); err != nil {
			t.Logf("cleanup: drop database %q: %v", dbName, err)
		}
	})

	testURL := rewriteDBName(adminURL, dbName)
	testDB, err := sql.Open("pgx", testURL)
	if err != nil {
		t.Fatalf("open test sql.DB: %v", err)
	}

	drv := entsql.OpenDB(dialect.Postgres, testDB)
	client := ent.NewClient(ent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("ent schema create: %v", err)
	}
	return client
}

// rewriteDBName replaces the path segment (the database name) in a
// postgres:// URL. testcontainers-go returns a URL pointing at the default
// db; we need to redirect each test's client at its freshly-created one.
func rewriteDBName(url, dbName string) string {
	// Simple string surgery is safer than url.Parse for postgres URLs with
	// optional query strings — pgx accepts both "postgres://" and "pgx://".
	slash := strings.LastIndex(url, "/")
	if slash == -1 {
		return url
	}
	prefix := url[:slash+1]
	// Keep any query suffix that comes after the database name.
	rest := url[slash+1:]
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
	}
	return prefix + dbName + query
}
