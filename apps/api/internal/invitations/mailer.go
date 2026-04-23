// Package invitations wires the mail-delivery side of the invite flow.
// The Mailer is a background worker that picks up newly-created (or
// re-invited) Invitation rows, renders the email, hands it to an
// email.Sender, and records the outcome on both the Invitation row
// (email_sent_at + token_plaintext clear) and a fresh EmailDelivery row
// (for audit + retry triage).
//
// The Mailer deliberately reads the Invitation table directly rather
// than consuming the outbox invitation.created event. That event exists
// for audit + future consumers (notifications dashboard, search-over-
// invites, etc.); keeping the token out of the event payload is what
// lets audit rows survive indefinitely without carrying a secret. See
// ADR-0013 for the rationale.
package invitations

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/emaildelivery"
	entinvitation "github.com/open-huddle/huddle/apps/api/ent/invitation"
	"github.com/open-huddle/huddle/apps/api/internal/email"
)

const (
	defaultMailerInterval  = 5 * time.Second
	defaultMailerBatchSize = 50
)

// Mailer sends pending invite emails. Instances are safe for concurrent
// use across replicas: the poll query uses SELECT ... FOR UPDATE SKIP
// LOCKED on Postgres so each replica claims a disjoint slice.
type Mailer struct {
	client      *ent.Client
	sender      email.Sender
	logger      *slog.Logger
	fromAddress string
	fromName    string
	linkBaseURL string

	interval  time.Duration
	batchSize int
	dialect   string
}

// Option configures a Mailer; zero options yields production defaults.
type Option func(*Mailer)

// WithInterval sets the poll cadence.
func WithInterval(d time.Duration) Option {
	return func(m *Mailer) { m.interval = d }
}

// WithBatchSize caps the number of emails processed per tick.
func WithBatchSize(n int) Option {
	return func(m *Mailer) { m.batchSize = n }
}

// WithDialect enables FOR UPDATE SKIP LOCKED on the poll query for
// Postgres. Omitted in unit tests (SQLite path).
func WithDialect(d string) Option {
	return func(m *Mailer) { m.dialect = d }
}

// NewMailer returns a Mailer that sends invite emails via the given
// Sender. fromAddress + fromName go on every outgoing message's From
// header; linkBaseURL is the public-facing URL the token gets appended
// to in the email body.
func NewMailer(
	client *ent.Client,
	sender email.Sender,
	logger *slog.Logger,
	fromAddress, fromName, linkBaseURL string,
	opts ...Option,
) *Mailer {
	m := &Mailer{
		client:      client,
		sender:      sender,
		logger:      logger,
		fromAddress: fromAddress,
		fromName:    fromName,
		linkBaseURL: linkBaseURL,
		interval:    defaultMailerInterval,
		batchSize:   defaultMailerBatchSize,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Run polls until ctx is cancelled. Mirrors the shape of outbox.Publisher
// and search.Indexer — drain once at startup so an invite queued while
// the process was down goes out immediately, then tick.
func (m *Mailer) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	if err := m.SendBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Warn("invitations-mailer: initial drain", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.SendBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
				m.logger.Warn("invitations-mailer: send batch", "err", err)
			}
		}
	}
}

// SendBatch processes one batch of pending invitations. Exported so
// tests can drive the loop deterministically.
//
// Pending means email_sent_at IS NULL AND accepted_at IS NULL AND
// expires_at > now. Per-row failures are logged + recorded as
// EmailDelivery rows with status=failed; they'll be picked up again
// next tick until the invite expires.
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

	q := tx.Invitation.Query().
		Where(
			entinvitation.EmailSentAtIsNil(),
			entinvitation.AcceptedAtIsNil(),
			entinvitation.ExpiresAtGT(time.Now()),
		).
		Order(ent.Asc(entinvitation.FieldCreatedAt)).
		Limit(m.batchSize)
	if m.dialect == dialect.Postgres {
		q = q.ForUpdate(sql.WithLockAction(sql.SkipLocked))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return fmt.Errorf("query pending invites: %w", err)
	}

	for _, inv := range rows {
		if err := m.sendOne(ctx, tx, inv); err != nil {
			// Individual failures don't abort the batch. sendOne already
			// recorded a failed EmailDelivery row for audit.
			m.logger.Warn("invitations-mailer: send",
				"err", err, "invite_id", inv.ID)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// sendOne renders and dispatches a single invitation. Returns a non-nil
// error on transport failure so the batch loop can log it with context;
// the caller does NOT abort the whole batch on one bad row.
func (m *Mailer) sendOne(ctx context.Context, tx *ent.Tx, inv *ent.Invitation) error {
	if inv.TokenPlaintext == nil || len(*inv.TokenPlaintext) == 0 {
		// Should never happen — InviteMember writes the token in the
		// same tx as the row. Treat as a structural bug: stamp
		// email_sent_at so we don't hot-loop, record the failure.
		reason := "invite has no plaintext token — data integrity bug"
		_ = recordDelivery(ctx, tx, inv.ID, inv.Email, emaildelivery.StatusFailed, reason)
		_, err := tx.Invitation.UpdateOne(inv).SetEmailSentAt(time.Now()).Save(ctx)
		return fmt.Errorf("invite %s: %s: %w", inv.ID, reason, err)
	}

	msg := email.Message{
		ToAddress:   inv.Email,
		FromAddress: m.fromAddress,
		FromName:    m.fromName,
		Subject:     "You've been invited to an Open Huddle organization",
		BodyText:    m.renderBody(inv),
	}

	if err := m.sender.Send(ctx, msg); err != nil {
		reason := truncateReason(err.Error())
		if delErr := recordDelivery(ctx, tx, inv.ID, inv.Email, emaildelivery.StatusFailed, reason); delErr != nil {
			// Don't mask the primary error.
			m.logger.Warn("invitations-mailer: record delivery failure",
				"err", delErr, "invite_id", inv.ID)
		}
		return fmt.Errorf("send: %w", err)
	}

	if err := recordDelivery(ctx, tx, inv.ID, inv.Email, emaildelivery.StatusSent, ""); err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	// Clear the plaintext token — the email's been sent, we don't need
	// to hold the secret any longer. Hash stays for accept-time lookup.
	if _, err := tx.Invitation.UpdateOne(inv).
		SetEmailSentAt(time.Now()).
		ClearTokenPlaintext().
		Save(ctx); err != nil {
		return fmt.Errorf("stamp email_sent_at: %w", err)
	}
	return nil
}

// renderBody builds the plain-text body. Format is deliberately simple
// — the goal is "a real email reaches the invitee"; richer templates
// (HTML + logo + branding) are a future PR.
func (m *Mailer) renderBody(inv *ent.Invitation) string {
	acceptURL := m.linkBaseURL
	sep := "?"
	if strings.Contains(acceptURL, "?") {
		sep = "&"
	}
	// Same encoding the AcceptInvitation handler expects on the wire.
	// Kept inline to avoid a dependency on the organization package from
	// this lower-level worker.
	acceptURL += sep + "token=" + base64.RawURLEncoding.EncodeToString(*inv.TokenPlaintext)

	var b strings.Builder
	b.WriteString("Hello,\n\n")
	b.WriteString("You have been invited to join an Open Huddle organization as a ")
	b.WriteString(string(inv.Role))
	b.WriteString(".\n\n")
	b.WriteString("To accept, visit:\n")
	b.WriteString(acceptURL)
	b.WriteString("\n\nThis link expires on ")
	b.WriteString(inv.ExpiresAt.UTC().Format(time.RFC1123))
	b.WriteString(".\n\nIf you weren't expecting this invitation, you can safely ignore this email.\n")
	return b.String()
}

func recordDelivery(ctx context.Context, tx *ent.Tx, inviteID uuid.UUID, recipient string, status emaildelivery.Status, reason string) error {
	create := tx.EmailDelivery.Create().
		SetInvitationID(inviteID).
		SetStatus(status).
		SetRecipient(recipient)
	if reason != "" {
		create = create.SetFailureReason(reason)
	}
	return create.Exec(ctx)
}

// truncateReason keeps failure_reason from storing a multi-MB SMTP error
// text. 2 KiB is plenty for diagnosis; operators with a bigger payload
// go to logs.
func truncateReason(s string) string {
	const maxLen = 2048
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
