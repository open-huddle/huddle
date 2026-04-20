// Command migrate generates Atlas-format versioned migrations from the Ent
// schema. Invoked by `make migrate-diff NAME=<desc>`.
//
// It relies on Ent's built-in Atlas integration: the diff is computed against
// a throwaway "dev database" so the result is deterministic and does not
// depend on the state of any real environment.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/schema"
	// Ent's NamedDiff calls sql.Open("postgres", url) with the driver name
	// hardcoded. The runtime API uses pgx (in internal/database), but the diff
	// tool needs something registered under "postgres" — lib/pq is the only
	// maintained driver that does so. Deprecated in general, fine for a
	// dev-only diff tool.
	_ "github.com/lib/pq"

	"github.com/open-huddle/huddle/apps/api/ent/migrate"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalln("usage: migrate <name>")
	}
	name := os.Args[1]

	devURL := os.Getenv("HUDDLE_DEV_DB_URL")
	if devURL == "" {
		log.Fatalln("HUDDLE_DEV_DB_URL must be set to a throwaway Postgres URL (Atlas uses it to compute schema diffs)")
	}

	dir, err := atlasmigrate.NewLocalDir("migrations")
	if err != nil {
		log.Fatalf("open migrations dir: %v", err)
	}

	opts := []schema.MigrateOption{
		schema.WithDir(dir),
		schema.WithMigrationMode(schema.ModeReplay),
		schema.WithDialect(dialect.Postgres),
		schema.WithFormatter(atlasmigrate.DefaultFormatter),
	}

	if err := migrate.NamedDiff(context.Background(), devURL, name, opts...); err != nil {
		log.Fatalf("diff: %v", err)
	}
	fmt.Printf("wrote migration %q\n", name)
}
