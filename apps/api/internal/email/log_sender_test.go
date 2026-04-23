package email_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/open-huddle/huddle/apps/api/internal/email"
)

// newCapturingLogger returns a LogSender backed by a buffered slog JSON
// handler so tests can assert on the encoded log record.
func newCapturingLogger() (*email.LogSender, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return email.NewLogSender(slog.New(slog.NewJSONHandler(buf, nil))), buf
}

func TestLogSender_SendRecordsAllFields(t *testing.T) {
	t.Parallel()
	sender, buf := newCapturingLogger()

	msg := email.Message{
		ToAddress:   "bob@example.test",
		FromAddress: "noreply@open-huddle.local",
		FromName:    "Open Huddle",
		Subject:     "Hello",
		BodyText:    "body contents here",
	}
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		`"msg":"email.send"`,
		`"email.from":"noreply@open-huddle.local"`,
		`"email.to":"bob@example.test"`,
		`"email.subject":"Hello"`,
		`"email.body":"body contents here"`,
	} {
		if !contains(out, want) {
			t.Errorf("log output missing %q; got: %s", want, out)
		}
	}
}

func TestLogSender_RejectsInvalidMessage(t *testing.T) {
	t.Parallel()
	sender, _ := newCapturingLogger()

	err := sender.Send(context.Background(), email.Message{
		// Missing ToAddress, FromAddress, Subject, BodyText.
	})
	if !errors.Is(err, email.ErrInvalidMessage) {
		t.Errorf("want ErrInvalidMessage, got %v", err)
	}
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && len(hay) > 0 && bytes.Contains([]byte(hay), []byte(needle))
}
