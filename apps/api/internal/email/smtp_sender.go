package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTPConfig is the runtime shape the SMTPSender needs. It mirrors
// config.SMTP so callers can construct the Sender without an import
// cycle; the cmd/api wiring code is responsible for the copy.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	// StartTLS when true runs a STARTTLS upgrade on the plain connection
	// before AUTH. Keep on for any relay that isn't on-box.
	StartTLS bool
}

// SMTPSender delivers via a classic submission-port SMTP relay. No queue,
// no retry — invitations.Mailer handles retry by polling Invitation
// rows whose email_sent_at stays NULL.
//
// Connection is opened per-Send. This is intentional: invite volume is
// low, a stale persistent connection causes more operational pain than
// it saves, and a short-lived connection means credentials don't sit in
// process memory longer than one round trip.
type SMTPSender struct {
	cfg     SMTPConfig
	timeout time.Duration
}

// NewSMTPSender validates the config and returns a Sender. Fails fast at
// construction so a misconfigured deployment surfaces on startup, not
// on the first invitation.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if cfg.Host == "" {
		return nil, errors.New("smtp: host is required")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("smtp: invalid port %d", cfg.Port)
	}
	return &SMTPSender{cfg: cfg, timeout: 15 * time.Second}, nil
}

func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if err := Validate(msg); err != nil {
		return err
	}

	// Honour ctx for the dial so a caller-supplied deadline bounds the
	// whole send. net/smtp itself is blocking — a failed remote costs us
	// up to s.timeout per Send.
	dialer := &net.Dialer{Timeout: s.timeout}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	// net/smtp.NewClient takes ownership of conn; its Close closes conn.
	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: handshake: %w", err)
	}
	defer func() { _ = client.Close() }()

	if s.cfg.StartTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
			if err := client.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("smtp: starttls: %w", err)
			}
		} else {
			return errors.New("smtp: starttls requested but server does not advertise it")
		}
	}

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}

	if err := client.Mail(msg.FromAddress); err != nil {
		return fmt.Errorf("smtp: MAIL FROM %s: %w", msg.FromAddress, err)
	}
	if err := client.Rcpt(msg.ToAddress); err != nil {
		return fmt.Errorf("smtp: RCPT TO %s: %w", msg.ToAddress, err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write([]byte(render(msg))); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: close body: %w", err)
	}
	if err := client.Quit(); err != nil {
		// Not fatal — the server accepted DATA; Quit is best-effort.
		// Log-friendly wrap so the call site can see it.
		return fmt.Errorf("smtp: quit: %w", err)
	}
	return nil
}

// render produces the RFC 5322 message (headers + body). Plain text
// only; no MIME parts. A future HTML-email PR will wrap this in a
// multipart/alternative layer.
func render(msg Message) string {
	var b strings.Builder
	writeHeader(&b, "From", formatAddress(msg.FromName, msg.FromAddress))
	writeHeader(&b, "To", formatAddress(msg.ToName, msg.ToAddress))
	writeHeader(&b, "Subject", msg.Subject)
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", "text/plain; charset=\"utf-8\"")
	b.WriteString("\r\n")
	b.WriteString(msg.BodyText)
	if !strings.HasSuffix(msg.BodyText, "\r\n") {
		b.WriteString("\r\n")
	}
	return b.String()
}

func writeHeader(b *strings.Builder, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

// formatAddress returns either "Name <addr>" or just "addr" depending on
// whether name is set. No quoting of the name — we control every callsite
// and the config validator rejects commas and angle brackets.
func formatAddress(name, addr string) string {
	if name == "" {
		return addr
	}
	return fmt.Sprintf("%s <%s>", name, addr)
}
