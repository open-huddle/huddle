package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

const (
	defaultIndexerInterval  = 2 * time.Second
	defaultIndexerBatchSize = 200

	// Event types the indexer acts on. message.created writes a fresh
	// doc, message.edited upserts over the same _id (message UUID,
	// per ADR-0016), message.deleted removes the doc. Other event
	// types (channel.created, invitation.created) are stamp-only —
	// they go through the default case below.
	eventTypeMessageCreated = "message.created"
	eventTypeMessageEdited  = "message.edited"
	eventTypeMessageDeleted = "message.deleted"
)

// Indexer mirrors OutboxEvent rows of type message.created into OpenSearch.
// Same polling shape as audit.Consumer: one query per tick for un-indexed
// rows, per-row upsert, stamp indexed_at on success.
type Indexer struct {
	client *ent.Client
	search Client
	logger *slog.Logger

	interval  time.Duration
	batchSize int
	// dialect gates FOR UPDATE SKIP LOCKED — same story as outbox.Publisher.
	dialect string
}

// IndexerOption configures an Indexer; zero options yields production defaults.
type IndexerOption func(*Indexer)

// WithIndexerInterval sets the poll cadence. Shorter intervals reduce
// search staleness at the cost of more idle queries.
func WithIndexerInterval(d time.Duration) IndexerOption {
	return func(i *Indexer) { i.interval = d }
}

// WithIndexerBatchSize caps the rows processed in one tick.
func WithIndexerBatchSize(n int) IndexerOption {
	return func(i *Indexer) { i.batchSize = n }
}

// WithIndexerDialect enables FOR UPDATE SKIP LOCKED on the claim query
// when the caller is running against Postgres. Omitted in tests.
func WithIndexerDialect(d string) IndexerOption {
	return func(i *Indexer) { i.dialect = d }
}

// NewIndexer wires an Indexer against the given ent client and search
// backend. The backend is expected to be ready — EnsureIndex should run at
// startup before the first tick.
func NewIndexer(client *ent.Client, search Client, logger *slog.Logger, opts ...IndexerOption) *Indexer {
	i := &Indexer{
		client:    client,
		search:    search,
		logger:    logger,
		interval:  defaultIndexerInterval,
		batchSize: defaultIndexerBatchSize,
	}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// Run polls until ctx is cancelled. Errors inside a batch are logged and
// the loop continues — this is a background worker, not a request path.
func (i *Indexer) Run(ctx context.Context) {
	ticker := time.NewTicker(i.interval)
	defer ticker.Stop()

	// Drain once immediately so startup doesn't wait an interval to pick up
	// rows enqueued while the worker was down.
	if err := i.IndexBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		i.logger.Warn("search-indexer: initial drain", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := i.IndexBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
				i.logger.Warn("search-indexer: index batch", "err", err)
			}
		}
	}
}

// IndexBatch does one iteration. Exported so tests can drive the loop
// deterministically.
//
// Wrapped in a transaction so the SELECT can hold FOR UPDATE SKIP LOCKED
// on Postgres — same multi-replica claim pattern as outbox.Publisher.
// OpenSearch write runs under the lock; commit releases it. OpenSearch
// upserts by outbox event UUID anyway, so the worst a retry does is
// rewrite the same document.
func (i *Indexer) IndexBatch(ctx context.Context) error {
	tx, err := i.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Poll ALL un-indexed rows, not just message.created. indexed_at is
	// outbox.GC's "has the indexer finished with this row?" marker — if we
	// only stamped message.created events, other event types (invitations,
	// future notifications) would never satisfy the GC predicate and the
	// outbox would grow unbounded. For non-message events the stamp is
	// load-bearing without triggering an OpenSearch write.
	q := tx.OutboxEvent.Query().
		Where(outboxevent.IndexedAtIsNil()).
		Order(ent.Asc(outboxevent.FieldCreatedAt)).
		Limit(i.batchSize)
	if i.dialect == dialect.Postgres {
		q = q.ForUpdate(sql.WithLockAction(sql.SkipLocked))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return fmt.Errorf("query un-indexed: %w", err)
	}

	for _, row := range rows {
		switch row.EventType {
		case eventTypeMessageCreated, eventTypeMessageEdited:
			// Both create and edit upsert by message_id (ADR-0016).
			// Create writes a fresh doc; edit overwrites the body and
			// mentions. Same code path — differences are in the outbox
			// payload, and the indexer doesn't care which.
			doc, err := docFromOutbox(row)
			if err != nil {
				i.logger.Warn("search-indexer: decode", "err", err, "outbox_id", row.ID)
				if stampErr := stampInTx(ctx, tx, row.ID); stampErr != nil {
					i.logger.Warn("search-indexer: stamp after decode error", "err", stampErr, "outbox_id", row.ID)
				}
				continue
			}
			if err := i.search.IndexMessage(ctx, doc); err != nil {
				i.logger.Warn("search-indexer: index", "err", err, "outbox_id", row.ID)
				continue
			}
		case eventTypeMessageDeleted:
			// Pull the message id out of the payload (delete events ship
			// just the id + channel id) and remove the doc.
			msgID, err := messageIDFromOutbox(row)
			if err != nil {
				i.logger.Warn("search-indexer: decode delete", "err", err, "outbox_id", row.ID)
				if stampErr := stampInTx(ctx, tx, row.ID); stampErr != nil {
					i.logger.Warn("search-indexer: stamp after decode error", "err", stampErr, "outbox_id", row.ID)
				}
				continue
			}
			if err := i.search.DeleteMessage(ctx, msgID); err != nil {
				i.logger.Warn("search-indexer: delete", "err", err, "outbox_id", row.ID)
				continue
			}
		default:
			// Other consumers handle these events. The indexer's job is
			// just to mark them "evaluated" so outbox.GC can proceed.
			if err := stampInTx(ctx, tx, row.ID); err != nil {
				i.logger.Warn("search-indexer: stamp non-message",
					"err", err, "outbox_id", row.ID, "event_type", row.EventType)
			}
			continue
		}
		if err := stampInTx(ctx, tx, row.ID); err != nil {
			// Failure to stamp means we'll re-process next tick — safe
			// because _id is the message UUID (upsert) and delete is
			// idempotent.
			i.logger.Warn("search-indexer: stamp", "err", err, "outbox_id", row.ID)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

func stampInTx(ctx context.Context, tx *ent.Tx, id uuid.UUID) error {
	return tx.OutboxEvent.UpdateOneID(id).
		SetIndexedAt(time.Now()).
		Exec(ctx)
}

// messageIDFromOutbox pulls the message id out of a message.deleted
// outbox payload. Delete events don't carry full doc context — only the
// id and channel — so this helper short-circuits the richer docFromOutbox
// decode.
func messageIDFromOutbox(row *ent.OutboxEvent) (uuid.UUID, error) {
	var m huddlev1.Message
	if err := proto.Unmarshal(row.Payload, &m); err != nil {
		return uuid.Nil, fmt.Errorf("unmarshal message payload: %w", err)
	}
	id, err := uuid.Parse(m.Id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("message.id: %w", err)
	}
	return id, nil
}

// docFromOutbox decodes the message.created or message.edited payload
// into a MessageDoc, combining protobuf fields
// (id/channel_id/author_id/body/created_at) with the denormalized
// organization_id stamped on the outbox row.
func docFromOutbox(row *ent.OutboxEvent) (MessageDoc, error) {
	if row.OrganizationID == nil {
		return MessageDoc{}, errors.New("outbox row has no organization_id")
	}
	var m huddlev1.Message
	if err := proto.Unmarshal(row.Payload, &m); err != nil {
		return MessageDoc{}, fmt.Errorf("unmarshal message payload: %w", err)
	}
	id, err := uuid.Parse(m.Id)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("message.id: %w", err)
	}
	cid, err := uuid.Parse(m.ChannelId)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("message.channel_id: %w", err)
	}
	aid, err := uuid.Parse(m.AuthorId)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("message.author_id: %w", err)
	}
	if m.CreatedAt == nil {
		return MessageDoc{}, errors.New("message.created_at missing")
	}
	return MessageDoc{
		ID:             id,
		ChannelID:      cid,
		OrganizationID: *row.OrganizationID,
		AuthorID:       aid,
		Body:           m.Body,
		CreatedAt:      m.CreatedAt.AsTime(),
	}, nil
}
