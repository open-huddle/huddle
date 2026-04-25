package config_test

import (
	"strings"
	"testing"

	"github.com/open-huddle/huddle/apps/api/internal/config"
)

// TestLoad_OutboxPublisherDriver_Default ensures a fresh Load() picks
// the in-process publisher. A regression here would silently disable
// realtime fan-out for every existing deployment that doesn't set the
// new env var.
func TestLoad_OutboxPublisherDriver_Default(t *testing.T) {
	t.Setenv("HUDDLE_OUTBOX_PUBLISHER_DRIVER", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Outbox.Publisher.Driver; got != config.OutboxPublisherDriverInProcess {
		t.Errorf("Driver = %q, want %q (the safe default for existing deployments)",
			got, config.OutboxPublisherDriverInProcess)
	}
}

// TestLoad_OutboxPublisherDriver_None covers the Slice B cutover
// configuration: setting the driver to "none" disables the in-process
// publisher (operator runbook for switching to Debezium CDC).
func TestLoad_OutboxPublisherDriver_None(t *testing.T) {
	t.Setenv("HUDDLE_OUTBOX_PUBLISHER_DRIVER", config.OutboxPublisherDriverNone)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Outbox.Publisher.Driver; got != config.OutboxPublisherDriverNone {
		t.Errorf("Driver = %q, want %q", got, config.OutboxPublisherDriverNone)
	}
}

// TestLoad_OutboxPublisherDriver_RejectsUnknown is the load-bearing
// case for ADR-0018's "fail fast on a typo" guarantee. A typo'd value
// silently disabling the publisher would mean realtime Subscribe stops
// working with no obvious signal, which is exactly what this validation
// prevents.
func TestLoad_OutboxPublisherDriver_RejectsUnknown(t *testing.T) {
	t.Setenv("HUDDLE_OUTBOX_PUBLISHER_DRIVER", "in-process") // common typo: dash vs underscore

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load: expected error for unknown driver, got nil")
	}
	if !strings.Contains(err.Error(), "outbox.publisher.driver") {
		t.Errorf("Load error %q does not mention the offending key — operator can't tell what's wrong", err.Error())
	}
}
