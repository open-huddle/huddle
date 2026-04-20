package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Addr     string   `mapstructure:"addr"`
	Version  string   `mapstructure:"version"`
	Database Database `mapstructure:"database"`
	Valkey   Valkey   `mapstructure:"valkey"`
}

type Database struct {
	URL string `mapstructure:"url"`
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
	v.SetDefault("valkey.url", "redis://localhost:6379")

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
