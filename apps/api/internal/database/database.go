package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	// pgx/stdlib registers the "pgx" driver with database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/open-huddle/huddle/apps/api/ent"
)

// Config carries the knobs Open needs. Durations of 0 mean "no limit" (see
// database/sql docs), which is rarely what you want in production.
type Config struct {
	URL             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// DB bundles the raw *sql.DB (for health probes and future low-level access)
// and the Ent client that uses it. Callers should treat DB as the ownership
// boundary: close it once and both go away.
type DB struct {
	SQL *sql.DB
	Ent *ent.Client
}

// Open dials Postgres using the pgx driver and returns an Ent client on top.
// It pings the database before returning so a misconfigured URL fails fast at
// startup rather than on the first request.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	sqlDB, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	return &DB{SQL: sqlDB, Ent: ent.NewClient(ent.Driver(drv))}, nil
}

// Close shuts down the Ent client, which closes the underlying *sql.DB.
func (d *DB) Close() error {
	return d.Ent.Close()
}
