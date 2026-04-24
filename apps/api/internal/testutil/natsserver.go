//go:build integration

package testutil

import (
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
)

// NewEmbeddedNATS starts an in-process NATS server with JetStream
// enabled on a random port and returns its client URL. The server
// shuts down on test cleanup. JetStream storage lives in t.TempDir(),
// so each test starts with an empty stream catalog and never collides
// with another test's state.
//
// Behind the `integration` build tag because the dependency
// (nats-server/v2) is heavyweight — not something every contributor
// running plain `go test ./...` should have to compile.
func NewEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natstest.RunServer(&opts)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatalf("embedded nats: not ready within 5s")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}
