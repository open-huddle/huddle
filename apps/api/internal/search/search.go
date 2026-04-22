// Package search is the API's full-text search backend. It maintains an
// OpenSearch projection of message bodies (written by Indexer) and serves
// queries (read by internal/services/search).
//
// Handlers and the indexer both depend on the Client interface rather than
// the concrete OpenSearch type so tests can drop in a fake without pulling
// the opensearch-go client in. The indexer mirrors the audit.Consumer
// pattern: poll OutboxEvent rows that have not been indexed yet, write the
// projection, stamp indexed_at. Failure in the indexer never blocks Send.
package search

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrBackendUnavailable is returned by Client methods when the underlying
// OpenSearch is unreachable. Handlers should map it to
// connect.CodeUnavailable; the indexer retries on the next tick.
var ErrBackendUnavailable = errors.New("search: backend unavailable")

// MessageDoc is the projection shape the indexer writes and the handler
// reads. Fields mirror the denormalized columns we already store on
// OutboxEvent plus the body text extracted from the serialized Message
// proto payload. organization_id is repeated here (it's also on the outbox
// row) because the OpenSearch-side tenant filter depends on it; the index
// is the source of truth for authz filtering at query time.
type MessageDoc struct {
	ID             uuid.UUID
	ChannelID      uuid.UUID
	OrganizationID uuid.UUID
	AuthorID       uuid.UUID
	Body           string
	CreatedAt      time.Time
}

// MessageQuery describes a SearchMessages call after the handler has parsed
// the proto request and resolved authz. The Client implementation is
// responsible for translating this into an OpenSearch query; handlers do
// not build OpenSearch DSL directly.
type MessageQuery struct {
	OrganizationID uuid.UUID     // required; tenant isolation filter
	Query          string        // required; full-text
	ChannelIDs     []uuid.UUID   // optional; restrict to these channels
	After          time.Time     // optional; zero means no lower bound
	Before         time.Time     // optional; zero means no upper bound
	Limit          int           // already defaulted/clamped by the handler
	CursorValues   []interface{} // OpenSearch search_after tuple; empty means first page
}

// MessageHit is the projection of a single OpenSearch hit. Snippet is
// Markdown with matched tokens wrapped in **bold**; empty when the
// highlighter returned nothing for this hit. SortValues is the search_after
// tuple that would page past this hit — the handler encodes the last hit's
// SortValues into the opaque cursor it returns to clients.
type MessageHit struct {
	Doc        MessageDoc
	Snippet    string
	Score      float32
	SortValues []interface{}
}

// MessageResult bundles the hits for one query plus the cursor tuple of the
// last hit (nil when the page is incomplete or empty).
type MessageResult struct {
	Hits       []MessageHit
	NextCursor []interface{}
}

// Client is the narrow surface the indexer and handler depend on. A real
// implementation sits over opensearch-go; tests use a fake.
type Client interface {
	// EnsureIndex bootstraps the concrete index and the alias on first use.
	// Idempotent — safe to call at every API startup.
	EnsureIndex(ctx context.Context) error
	// IndexMessage upserts a single message projection. The OpenSearch
	// document _id is the outbox event UUID, which gives us natural
	// idempotency: the indexer retrying a row writes the same _id.
	IndexMessage(ctx context.Context, outboxEventID uuid.UUID, doc MessageDoc) error
	// SearchMessages runs a query and returns at most q.Limit hits. The
	// client owns translating MessageQuery to OpenSearch DSL.
	SearchMessages(ctx context.Context, q MessageQuery) (MessageResult, error)
}
