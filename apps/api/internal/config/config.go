package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Addr       string     `mapstructure:"addr"`
	Version    string     `mapstructure:"version"`
	Database   Database   `mapstructure:"database"`
	Valkey     Valkey     `mapstructure:"valkey"`
	Auth       Auth       `mapstructure:"auth"`
	Nats       Nats       `mapstructure:"nats"`
	OpenSearch OpenSearch `mapstructure:"opensearch"`
	Outbox     Outbox     `mapstructure:"outbox"`
	Invites    Invites    `mapstructure:"invites"`
	Email      Email      `mapstructure:"email"`
}

// Invites configures the email-invitation subsystem. Secret is the HMAC
// key the handler uses to hash the random token before storing it;
// operators MUST override the default in any deployment beyond local dev.
type Invites struct {
	// Secret is the HMAC-SHA256 key used to hash invite tokens at rest.
	// Set per-deployment. Rotating this value invalidates every
	// outstanding pending invitation — by design: a rotation is a
	// response to suspected token leakage.
	Secret string `mapstructure:"secret"`
	// LinkBaseURL is the public-facing URL the invite email points at.
	// The mailer appends `?token=<token>` before sending.
	LinkBaseURL string `mapstructure:"link_base_url"`
	// TTL is how long an invitation stays acceptable after creation.
	// Re-inviting resets the TTL.
	TTL time.Duration `mapstructure:"ttl"`
}

// Email configures the outbound email pipeline. Driver picks the concrete
// Sender implementation: "log" prints the rendered email to the API log
// (dev default — no SMTP needed to exercise the invite flow) or "smtp"
// dials an SMTP relay. FromAddress / FromName apply to both drivers.
type Email struct {
	// Driver is "log" or "smtp". Empty / unknown values fall back to "log".
	Driver      string `mapstructure:"driver"`
	FromAddress string `mapstructure:"from_address"`
	FromName    string `mapstructure:"from_name"`
	SMTP        SMTP   `mapstructure:"smtp"`
}

// SMTP is the knobs for the "smtp" driver. All fields are ignored when
// Driver is "log". Host+Port are required; Username+Password are required
// when the relay demands auth (most do). StartTLS enables opportunistic
// STARTTLS — prefer true on anything that isn't on-box.
type SMTP struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	StartTLS bool   `mapstructure:"start_tls"`
}

// Outbox configures the transactional-outbox workers. The publisher and
// audit/search consumers run on tight cadences tuned in code; operators
// typically only override Retention, which controls how long fully-stamped
// rows live in the outbox before GC reaps them.
type Outbox struct {
	// Retention is the minimum age before a fully-published, fully-audited,
	// fully-indexed outbox row is eligible for deletion. Longer is safer
	// (larger window to diagnose a bad publish) but keeps the table bigger.
	Retention time.Duration `mapstructure:"retention"`
}

// Nats configures the JetStream connection used for realtime fan-out and
// (future) audit / search / notifications consumers.
type Nats struct {
	URL string `mapstructure:"url"`
}

// OpenSearch configures the full-text search backend. The indexer worker
// writes message projections here and SearchService queries them. Index
// names use the alias-over-versioned-index pattern — callers always talk to
// the alias, reindex flips it.
type OpenSearch struct {
	URL string `mapstructure:"url"`
	// MessagesIndex is the alias that SearchService queries and the indexer
	// writes to. The concrete index name (e.g. huddle-messages-v1) is a
	// deployment detail owned by whoever bootstraps the cluster.
	MessagesIndex string `mapstructure:"messages_index"`
}

// Auth controls OIDC token verification. Issuer is the full URL of the realm
// (ends in "/realms/<name>" for Keycloak). Audience is the claim the API
// requires in every access token — usually the logical API identifier, not
// the client ID that issued the token.
type Auth struct {
	IssuerURL string `mapstructure:"issuer_url"`
	Audience  string `mapstructure:"audience"`
}

// Database controls the Postgres client. Pool settings default to values that
// are safe for a single API replica sharing a Postgres with other services;
// operators tune them per-deployment.
type Database struct {
	URL             string        `mapstructure:"url"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
}

type Valkey struct {
	URL string `mapstructure:"url"`
}

// Load resolves configuration from (highest precedence first):
//  1. Environment variables prefixed HUDDLE_ (e.g. HUDDLE_ADDR, HUDDLE_DATABASE_URL).
//  2. config.yaml in ./ or /etc/huddle/ if present.
//  3. Built-in defaults.
func Load() (*Config, error) {
	v := viper.New()

	v.SetDefault("addr", ":8080")
	v.SetDefault("version", "dev")
	v.SetDefault("database.url", "postgres://huddle:huddle@localhost:5432/huddle?sslmode=disable")
	v.SetDefault("database.max_open_conns", 25)
	v.SetDefault("database.max_idle_conns", 5)
	v.SetDefault("database.conn_max_lifetime", 30*time.Minute)
	v.SetDefault("database.conn_max_idle_time", 5*time.Minute)
	v.SetDefault("valkey.url", "redis://localhost:6379")
	v.SetDefault("auth.issuer_url", "http://localhost:8180/realms/huddle")
	v.SetDefault("auth.audience", "huddle-api")
	v.SetDefault("nats.url", "nats://localhost:4222")
	v.SetDefault("opensearch.url", "http://localhost:9200")
	v.SetDefault("opensearch.messages_index", "huddle-messages")
	v.SetDefault("outbox.retention", 24*time.Hour)
	// Dev-only default for invites.secret. Operators MUST override
	// HUDDLE_INVITES_SECRET in any non-dev deployment — running with this
	// literal value would let anyone with the source tree forge invite
	// tokens. The handler logs a warning at startup if the default is in
	// use (see cmd/api/main.go).
	v.SetDefault("invites.secret", "dev-only-invites-secret-change-in-prod")
	v.SetDefault("invites.link_base_url", "http://localhost:5173/accept-invite")
	v.SetDefault("invites.ttl", 7*24*time.Hour)
	v.SetDefault("email.driver", "log")
	v.SetDefault("email.from_address", "noreply@open-huddle.local")
	v.SetDefault("email.from_name", "Open Huddle")
	v.SetDefault("email.smtp.port", 587)
	v.SetDefault("email.smtp.start_tls", true)

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/huddle")

	v.SetEnvPrefix("HUDDLE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}
