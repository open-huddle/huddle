package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/XSAM/otelsql"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

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
//
// The driver is wrapped with otelsql so every Query/Exec/Begin/Commit becomes
// a span (when the global tracer is configured; a no-op tracer is the default
// and adds zero overhead). Connection-pool stats are also exposed as OTel
// metrics — these are the load-bearing signals when diagnosing a stuck pool.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	sqlDB, err := otelsql.Open("pgx", cfg.URL,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
	)
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
	// Connection-pool stats become OTel metrics — load-bearing when
	// diagnosing a stuck pool. The Registration handle is for explicit
	// Unregister, which we don't do (process lifetime == metric lifetime).
	if _, err := otelsql.RegisterDBStatsMetrics(sqlDB,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
	); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("register db stats metrics: %w", err)
	}
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	return &DB{SQL: sqlDB, Ent: ent.NewClient(ent.Driver(drv))}, nil
}

// Close shuts down the Ent client, which closes the underlying *sql.DB.
func (d *DB) Close() error {
	return d.Ent.Close()
}
