package notifications_test

import (
	"testing"

	"connectrpc.com/connect"

	entnotificationpreference "github.com/open-huddle/huddle/apps/api/ent/notificationpreference"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// TestGetPreferences_DefaultsWhenNoRows covers the industry-standard
// opt-out shape: a user who has never touched preferences gets
// email_enabled=true for every known kind.
func TestGetPreferences_DefaultsWhenNoRows(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.GetPreferences(ctx, connect.NewRequest(&huddlev1.NotificationServiceGetPreferencesRequest{}))
	if err != nil {
		t.Fatalf("GetPreferences: %v", err)
	}
	if len(resp.Msg.Preferences) != 1 {
		t.Fatalf("want 1 known kind, got %d", len(resp.Msg.Preferences))
	}
	if resp.Msg.Preferences[0].Kind != string(entnotificationpreference.KindMention) {
		t.Errorf("kind: want mention, got %q", resp.Msg.Preferences[0].Kind)
	}
	if !resp.Msg.Preferences[0].EmailEnabled {
		t.Error("default must be email_enabled=true (opt-out model)")
	}
}

func TestSetPreference_UpsertsNewRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	resp, err := f.svc.SetPreference(ctx, connect.NewRequest(&huddlev1.NotificationServiceSetPreferenceRequest{
		Kind:         "mention",
		EmailEnabled: false,
	}))
	if err != nil {
		t.Fatalf("SetPreference: %v", err)
	}
	if resp.Msg.Preference.EmailEnabled {
		t.Error("response should reflect the new email_enabled=false")
	}

	// Row exists in DB with the new value.
	count, err := f.client.NotificationPreference.Query().Count(f.ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("want 1 preference row after SetPreference, got %d", count)
	}
}

func TestSetPreference_UpdatesExistingRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	// First set: opt out.
	if _, err := f.svc.SetPreference(ctx, connect.NewRequest(&huddlev1.NotificationServiceSetPreferenceRequest{
		Kind:         "mention",
		EmailEnabled: false,
	})); err != nil {
		t.Fatalf("first SetPreference: %v", err)
	}

	// Second set: opt back in.
	resp, err := f.svc.SetPreference(ctx, connect.NewRequest(&huddlev1.NotificationServiceSetPreferenceRequest{
		Kind:         "mention",
		EmailEnabled: true,
	}))
	if err != nil {
		t.Fatalf("second SetPreference: %v", err)
	}
	if !resp.Msg.Preference.EmailEnabled {
		t.Error("second SetPreference should reflect the new email_enabled=true")
	}

	// Still exactly one row — upsert, not insert.
	count, err := f.client.NotificationPreference.Query().Count(f.ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("upsert should keep exactly 1 row, got %d", count)
	}
}

func TestSetPreference_RejectsUnknownKind(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := callerCtx(f.ctx, "alice")

	_, err := f.svc.SetPreference(ctx, connect.NewRequest(&huddlev1.NotificationServiceSetPreferenceRequest{
		Kind:         "reaction", // not yet a known kind
		EmailEnabled: false,
	}))
	if got := connectCode(t, err); got != connect.CodeInvalidArgument {
		t.Errorf("code: want InvalidArgument, got %v", got)
	}
}

// Preferences are per-caller. Setting a preference for one user must
// not affect another. Covers the "no cross-user surface" invariant
// listed in the proto comments.
func TestGetPreferences_IsolatedPerCaller(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	aliceCtx := callerCtx(f.ctx, "alice")
	bobCtx := callerCtx(f.ctx, "bob")

	if _, err := f.svc.SetPreference(aliceCtx, connect.NewRequest(&huddlev1.NotificationServiceSetPreferenceRequest{
		Kind:         "mention",
		EmailEnabled: false,
	})); err != nil {
		t.Fatalf("alice SetPreference: %v", err)
	}

	// Bob should still see the default (enabled) — alice's row is not
	// visible to him.
	resp, err := f.svc.GetPreferences(bobCtx, connect.NewRequest(&huddlev1.NotificationServiceGetPreferencesRequest{}))
	if err != nil {
		t.Fatalf("bob GetPreferences: %v", err)
	}
	if !resp.Msg.Preferences[0].EmailEnabled {
		t.Error("bob should see the default (enabled) regardless of alice's preference")
	}
}
