package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Addr     string   `mapstructure:"addr"`
	Version  string   `mapstructure:"version"`
	Database Database `mapstructure:"database"`
	Valkey   Valkey   `mapstructure:"valkey"`
	Auth     Auth     `mapstructure:"auth"`
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
