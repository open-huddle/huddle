package email

import (
	"context"
	"log/slog"
)

// LogSender is the dev-default Sender. It writes the rendered email to
// the structured logger rather than delivering it — a contributor can
// exercise the full invite flow without setting up SMTP, and the
// `buf curl` + `grep` combination in local-development.md is how we
// demonstrate it works.
//
// Body text is logged at Info level under the `email.body` key. A CI
// job or a curious operator pipes the API's stdout to see what would
// have been sent; there is no separate on-disk spool.
type LogSender struct {
	logger *slog.Logger
}

// NewLogSender wraps the given logger. Passing nil falls back to
// slog.Default() — useful in tests where the caller doesn't care about
// the sink.
func NewLogSender(logger *slog.Logger) *LogSender {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogSender{logger: logger}
}

// Send logs the message fields and returns nil. The log-only sender
// never produces a transport error; a returned non-nil error can only
// come from Validate (e.g. a handler forgot to set a required field),
// which tests want to see surface.
func (s *LogSender) Send(_ context.Context, msg Message) error {
	if err := Validate(msg); err != nil {
		return err
	}
	s.logger.Info("email.send",
		"driver", "log",
		"email.from", msg.FromAddress,
		"email.to", msg.ToAddress,
		"email.subject", msg.Subject,
		"email.body", msg.BodyText,
	)
	return nil
}
