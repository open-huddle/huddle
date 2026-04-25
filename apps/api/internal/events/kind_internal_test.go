package events

import "testing"

// kindFromSubject is the single mapping from a NATS subject to the
// MessageEventKind clients dispatch on. The integration test in
// nats_integration_test.go exercises the wire path; this unit test
// covers the parsing edge cases (unknown verbs, malformed subjects)
// without bringing up a server.
func TestKindFromSubject(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    MessageEventKind
	}{
		{"created", "huddle.messages.created.abc-123", MessageEventCreated},
		{"edited", "huddle.messages.edited.abc-123", MessageEventEdited},
		{"deleted", "huddle.messages.deleted.abc-123", MessageEventDeleted},

		// Unknown verb in the third segment — future control frames or
		// a typo'd publisher must not silently masquerade as Created.
		{"unknown verb", "huddle.messages.heartbeat.abc-123", MessageEventUnknown},

		// Subject too short to even reach the verb segment. SplitN
		// returns < 3 parts, so we hit the early-return guard.
		{"too short, two segments", "huddle.messages", MessageEventUnknown},
		{"too short, one segment", "huddle", MessageEventUnknown},
		{"empty", "", MessageEventUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kindFromSubject(tt.subject); got != tt.want {
				t.Errorf("kindFromSubject(%q) = %v, want %v", tt.subject, got, tt.want)
			}
		})
	}
}
