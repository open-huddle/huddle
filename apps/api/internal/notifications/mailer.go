package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	entnotification "github.com/open-huddle/huddle/apps/api/ent/notification"
	entnotificationpreference "github.com/open-huddle/huddle/apps/api/ent/notificationpreference"
	entuser "github.com/open-huddle/huddle/apps/api/ent/user"
	"github.com/open-huddle/huddle/apps/api/internal/email"
)

const (
	defaultMailerInterval  = 5 * time.Second
	defaultMailerBatchSize = 50
)

// Mailer sends notification emails. Reads from the Notification table
// (not the outbox) so the consumer/mailer split mirrors the invitations
// pattern: the outbox-driven Consumer creates the in-app Notification
// row; the Mailer is the email transport layer on top.
//
// Opt-out semantics: a user with no NotificationPreference row for a
// given kind gets emailed. The poll query filters out only rows where
// the caller has an explicit `email_enabled = false` preference. That
// matches industry norm for @-mentions (Slack, GitHub, etc.).
type Mailer struct {
	client      *ent.Client
	sender      email.Sender
	logger      *slog.Logger
	fromAddress string
	fromName    string
	appBaseURL  string

	interval  time.Duration
	batchSize int
	dialect   string
}

// MailerOption tunes Mailer; zero options yields production defaults.
type MailerOption func(*Mailer)

// WithMailerInterval sets the poll cadence. 5s default — invite emails
// want to go fast (a signup is waiting on the URL), notification
// emails are background; tune per preference.
func WithMailerInterval(d time.Duration) MailerOption {
	return func(m *Mailer) { m.interval = d }
}

// WithMailerBatchSize caps the rows processed per tick.
func WithMailerBatchSize(n int) MailerOption {
	return func(m *Mailer) { m.batchSize = n }
}

// WithMailerDialect enables FOR UPDATE SKIP LOCKED on the poll query
// for Postgres. Omit in unit tests (SQLite).
func WithMailerDialect(d string) MailerOption {
	return func(m *Mailer) { m.dialect = d }
}

// NewMailer wires a Mailer against the given ent client and Sender.
func NewMailer(
	client *ent.Client,
	sender email.Sender,
	logger *slog.Logger,
	fromAddress, fromName, appBaseURL string,
	opts ...MailerOption,
) *Mailer {
	m := &Mailer{
		client:      client,
		sender:      sender,
		logger:      logger,
		fromAddress: fromAddress,
		fromName:    fromName,
		appBaseURL:  appBaseURL,
		interval:    defaultMailerInterval,
		batchSize:   defaultMailerBatchSize,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Run polls until ctx is cancelled. Same shape as every other
// outbox-family worker: drain once at startup, then tick.
func (m *Mailer) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	if err := m.SendBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Warn("notifications-mailer: initial drain", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.SendBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
				m.logger.Warn("notifications-mailer: send batch", "err", err)
			}
		}
	}
}

// SendBatch does one tick. Exported so tests can drive the loop
// deterministically.
//
// Filters out notifications whose recipient has explicitly opted out
// (email_enabled = false on a NotificationPreference row matching the
// notification's kind). Missing preference = enabled (the default).
func (m *Mailer) SendBatch(ctx context.Context) error {
	tx, err := m.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// "Un-emailed notifications, source=message_created (edits stay
	// in-app only per ADR-0016), whose recipient hasn't opted out of
	// email for this kind." Expressed as: source = message_created,
	// no matching preference row with email_enabled = false.
	q := tx.Notification.Query().
		Where(
			entnotification.EmailedAtIsNil(),
			entnotification.SourceEQ(entnotification.SourceMessageCreated),
			entnotification.Not(
				entnotification.HasRecipientWith(
					entuser.HasNotificationPreferencesWith(
						entnotificationpreference.EmailEnabledEQ(false),
						// Match preference kind to notification kind.
						// ent doesn't let us reference the outer row's
						// kind in the subquery, so we evaluate both
						// kinds explicitly — there's only one today.
						entnotificationpreference.KindEQ(entnotificationpreference.KindMention),
					),
				),
			),
		).
		Order(ent.Asc(entnotification.FieldCreatedAt)).
		Limit(m.batchSize)
	if m.dialect == dialect.Postgres {
		q = q.ForUpdate(sql.WithLockAction(sql.SkipLocked))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return fmt.Errorf("query pending: %w", err)
	}

	for _, n := range rows {
		if err := m.sendOne(ctx, tx, n); err != nil {
			m.logger.Warn("notifications-mailer: send",
				"err", err, "notification_id", n.ID)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// sendOne renders and dispatches one notification email. On success,
// stamps emailed_at so the row is filtered out of the next tick. On
// failure, leaves emailed_at nil and returns the error — the batch loop
// logs and the next tick retries.
func (m *Mailer) sendOne(ctx context.Context, tx *ent.Tx, n *ent.Notification) error {
	recipient, err := tx.User.Get(ctx, n.RecipientUserID)
	if err != nil {
		return fmt.Errorf("load recipient: %w", err)
	}
	subject, body, err := m.render(ctx, tx, n, recipient)
	if err != nil {
		// Structural (missing message, bad uuid) vs transient is hard
		// to tell here; stamp emailed_at either way so we don't hot-
		// loop on a bad row. Prod investigates via the log.
		m.logger.Warn("notifications-mailer: render",
			"err", err, "notification_id", n.ID)
		if _, stampErr := tx.Notification.UpdateOne(n).SetEmailedAt(time.Now()).Save(ctx); stampErr != nil {
			m.logger.Warn("notifications-mailer: stamp after render error",
				"err", stampErr, "notification_id", n.ID)
		}
		return err
	}

	if err := m.sender.Send(ctx, email.Message{
		ToAddress:   recipient.Email,
		ToName:      recipient.DisplayName,
		FromAddress: m.fromAddress,
		FromName:    m.fromName,
		Subject:     subject,
		BodyText:    body,
	}); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	if _, err := tx.Notification.UpdateOne(n).SetEmailedAt(time.Now()).Save(ctx); err != nil {
		return fmt.Errorf("stamp emailed_at: %w", err)
	}
	return nil
}

// render builds the subject + body for a notification email. Plain text
// only for MVP; HTML lands with a broader template-system PR.
func (m *Mailer) render(ctx context.Context, tx *ent.Tx, n *ent.Notification, recipient *ent.User) (string, string, error) {
	if n.MessageID == nil {
		return "", "", errors.New("notification has no message_id; non-message kinds not yet supported")
	}
	msg, err := tx.Message.Get(ctx, *n.MessageID)
	if err != nil {
		return "", "", fmt.Errorf("load message: %w", err)
	}
	sender, err := tx.User.Get(ctx, msg.AuthorID)
	if err != nil {
		return "", "", fmt.Errorf("load sender: %w", err)
	}
	channel, err := tx.Channel.Get(ctx, msg.ChannelID)
	if err != nil {
		return "", "", fmt.Errorf("load channel: %w", err)
	}

	senderName := sender.DisplayName
	if senderName == "" {
		senderName = sender.Email
	}

	subject := fmt.Sprintf("%s mentioned you in #%s", senderName, channel.Slug)

	var b strings.Builder
	fmt.Fprintf(&b, "Hello %s,\n\n", recipient.DisplayName)
	fmt.Fprintf(&b, "%s mentioned you in #%s:\n\n", senderName, channel.Slug)
	b.WriteString(excerpt(msg.Body))
	b.WriteString("\n\nOpen in Huddle: ")
	b.WriteString(m.messageURL(channel.ID, msg.ID))
	b.WriteString("\n\nTo stop receiving these emails, update your notification preferences in Huddle.\n")
	return subject, b.String(), nil
}

// messageURL composes the "view in app" link. Adopts a stable shape
// (/channels/<id>#message-<id>) so the web app can scroll-to-message
// on load. app.base_url sets the origin.
func (m *Mailer) messageURL(channelID, messageID uuid.UUID) string {
	base := strings.TrimRight(m.appBaseURL, "/")
	return fmt.Sprintf("%s/channels/%s#message-%s", base, channelID, messageID)
}

// excerpt returns a short preview of the message body. Keeps emails
// scannable even when the body is long; the "view in app" link leads
// to the full text.
func excerpt(body string) string {
	const maxExcerpt = 280
	trimmed := strings.TrimSpace(body)
	if len(trimmed) <= maxExcerpt {
		return trimmed
	}
	return trimmed[:maxExcerpt] + "…"
}
