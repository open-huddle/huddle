// Package email is the API's outbound-mail abstraction. A Sender delivers
// a Message to one recipient; handlers and workers depend on the Sender
// interface so tests can inject a fake, development can use the log-only
// sender, and production can wire SMTP — all without code changes on the
// call sites.
//
// Intentionally minimal: no HTML body, no attachments, no BCC. The first
// consumer (invitations.Mailer) needs only plain text; richer emails
// land alongside their use cases.
package email

import (
	"context"
	"errors"
)

// Message is the smallest shape an outbound email needs. ToAddress is a
// single recipient — if we ever need fan-out, we call Send N times rather
// than grow the Message surface (the Sender implementations stay simple,
// and the call-site decides retry / concurrency per-recipient).
type Message struct {
	ToAddress   string // bare address, no "Name <addr>" wrapping
	ToName      string // optional; used for the From-style "Name <addr>" header
	FromAddress string
	FromName    string
	Subject     string
	BodyText    string
}

// ErrInvalidMessage is returned when a Message is missing a required
// field (to/from/subject/body). Separate from transport errors so tests
// can distinguish "bad message" from "SMTP down".
var ErrInvalidMessage = errors.New("email: invalid message")

// Sender delivers one message. Implementations must be safe for concurrent
// use. A returned error is treated as a transient transport failure by
// invitations.Mailer; to signal "bad message, do not retry," wrap
// ErrInvalidMessage.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Validate does the shared pre-flight every Sender would otherwise have
// to re-implement. Call this before handing a Message to the transport.
func Validate(msg Message) error {
	if msg.ToAddress == "" {
		return errors.Join(ErrInvalidMessage, errors.New("to_address is required"))
	}
	if msg.FromAddress == "" {
		return errors.Join(ErrInvalidMessage, errors.New("from_address is required"))
	}
	if msg.Subject == "" {
		return errors.Join(ErrInvalidMessage, errors.New("subject is required"))
	}
	if msg.BodyText == "" {
		return errors.Join(ErrInvalidMessage, errors.New("body_text is required"))
	}
	return nil
}
