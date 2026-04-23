// Package notifications wires the in-app notifications side of the
// platform. The Consumer polls outbox rows, decodes the message.created
// events, and materializes one Notification per (mentioned user, kind)
// tuple. NotificationService (internal/services/notifications) reads
// the resulting rows for the caller's inbox.
//
// The Consumer is the fourth+ outbox-driven worker: same polling shape
// as audit.Consumer / search.Indexer / invitations.Mailer, same
// FOR UPDATE SKIP LOCKED claim on Postgres, stamps notified_at on every
// row it evaluates so outbox.GC can eventually reap them.
package notifications

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
	entnotification "github.com/open-huddle/huddle/apps/api/ent/notification"
	"github.com/open-huddle/huddle/apps/api/ent/outboxevent"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

const (
	defaultConsumerInterval  = 2 * time.Second
	defaultConsumerBatchSize = 200

	eventTypeMessageCreated = "message.created"
)

// Consumer materializes Notification rows from outbox events. One Consumer
// per API replica; SKIP LOCKED keeps concurrent replicas from duplicating
// work — the UNIQUE constraint on Notification(recipient, message, kind)
// is the belt-and-braces idempotency guard.
type Consumer struct {
	client *ent.Client
	logger *slog.Logger

	interval  time.Duration
	batchSize int
	dialect   string
}

// Option tunes a Consumer; zero options yields production defaults.
type Option func(*Consumer)

// WithInterval sets the poll cadence.
func WithInterval(d time.Duration) Option {
	return func(c *Consumer) { c.interval = d }
}

// WithBatchSize caps how many rows one tick evaluates.
func WithBatchSize(n int) Option {
	return func(c *Consumer) { c.batchSize = n }
}

// WithDialect enables FOR UPDATE SKIP LOCKED on Postgres. Omit in unit
// tests against SQLite.
func WithDialect(d string) Option {
	return func(c *Consumer) { c.dialect = d }
}

// NewConsumer wires a Consumer against the given ent client.
func NewConsumer(client *ent.Client, logger *slog.Logger, opts ...Option) *Consumer {
	c := &Consumer{
		client:    client,
		logger:    logger,
		interval:  defaultConsumerInterval,
		batchSize: defaultConsumerBatchSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run polls until ctx is cancelled. Errors inside a batch are logged; the
// loop doesn't abort on per-row failures.
func (c *Consumer) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	if err := c.ConsumeBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.logger.Warn("notifications: initial drain", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.ConsumeBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.logger.Warn("notifications: consume batch", "err", err)
			}
		}
	}
}

// ConsumeBatch does one tick. Exported so tests drive the loop
// deterministically.
//
// Poll rule: every un-notified outbox row, regardless of event type. For
// message.created we decode and fan out Notification rows; for every
// other event type we stamp notified_at without further work. Stamping
// non-message rows is what keeps outbox.GC moving — same rationale as the
// indexer's post-ADR-0013 behavior.
func (c *Consumer) ConsumeBatch(ctx context.Context) error {
	tx, err := c.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	q := tx.OutboxEvent.Query().
		Where(outboxevent.NotifiedAtIsNil()).
		Order(ent.Asc(outboxevent.FieldCreatedAt)).
		Limit(c.batchSize)
	if c.dialect == dialect.Postgres {
		q = q.ForUpdate(sql.WithLockAction(sql.SkipLocked))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return fmt.Errorf("query un-notified: %w", err)
	}

	for _, row := range rows {
		if row.EventType != eventTypeMessageCreated {
			// No work for this consumer — just stamp so GC can proceed.
			if err := stampInTx(ctx, tx, row.ID); err != nil {
				c.logger.Warn("notifications: stamp non-message",
					"err", err, "outbox_id", row.ID, "event_type", row.EventType)
			}
			continue
		}

		mentions, msgID, channelID, orgID, err := decodeMessageCreated(row)
		if err != nil {
			// Structural: bad payload. Stamp anyway so we don't hot-loop;
			// the bad row is a bug to investigate out-of-band.
			c.logger.Warn("notifications: decode payload",
				"err", err, "outbox_id", row.ID)
			if stampErr := stampInTx(ctx, tx, row.ID); stampErr != nil {
				c.logger.Warn("notifications: stamp after decode error",
					"err", stampErr, "outbox_id", row.ID)
			}
			continue
		}

		for _, recipient := range mentions {
			if err := createNotification(ctx, tx, recipient, msgID, channelID, orgID); err != nil {
				// UNIQUE violation (retry after a crash, concurrent
				// replica that squeaked through before SKIP LOCKED) is
				// benign — the row we tried to insert already exists.
				// Any other error is logged; we keep going with the
				// batch so one bad (recipient, message) pair doesn't
				// starve the rest.
				c.logger.Warn("notifications: create",
					"err", err, "outbox_id", row.ID, "recipient", recipient)
			}
		}

		if err := stampInTx(ctx, tx, row.ID); err != nil {
			c.logger.Warn("notifications: stamp after fan-out",
				"err", err, "outbox_id", row.ID)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// decodeMessageCreated extracts the fan-out inputs from a message.created
// outbox row: the mention set (recipients), the source message id, the
// channel id, and the organization id (from the denormalized outbox
// column, since the Message proto doesn't carry org).
func decodeMessageCreated(row *ent.OutboxEvent) ([]uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, error) {
	var m huddlev1.Message
	if err := proto.Unmarshal(row.Payload, &m); err != nil {
		return nil, uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("unmarshal: %w", err)
	}
	msgID, err := uuid.Parse(m.Id)
	if err != nil {
		return nil, uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("message.id: %w", err)
	}
	channelID, err := uuid.Parse(m.ChannelId)
	if err != nil {
		return nil, uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("message.channel_id: %w", err)
	}
	if row.OrganizationID == nil {
		return nil, uuid.Nil, uuid.Nil, uuid.Nil, errors.New("outbox row has no organization_id")
	}
	mentions := make([]uuid.UUID, 0, len(m.MentionUserIds))
	for _, s := range m.MentionUserIds {
		uid, err := uuid.Parse(s)
		if err != nil {
			return nil, uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("mention_user_id %q: %w", s, err)
		}
		mentions = append(mentions, uid)
	}
	return mentions, msgID, channelID, *row.OrganizationID, nil
}

func createNotification(ctx context.Context, tx *ent.Tx, recipient, msgID, channelID, orgID uuid.UUID) error {
	return tx.Notification.Create().
		SetRecipientUserID(recipient).
		SetKind(entnotification.KindMention).
		SetMessageID(msgID).
		SetChannelID(channelID).
		SetOrganizationID(orgID).
		Exec(ctx)
}

func stampInTx(ctx context.Context, tx *ent.Tx, id uuid.UUID) error {
	return tx.OutboxEvent.UpdateOneID(id).
		SetNotifiedAt(time.Now()).
		Exec(ctx)
}
