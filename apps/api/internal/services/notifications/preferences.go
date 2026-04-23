package notifications

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/open-huddle/huddle/apps/api/ent"
	entnotificationpreference "github.com/open-huddle/huddle/apps/api/ent/notificationpreference"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// knownKinds is the closed set of kinds GetPreferences returns defaults
// for. Adding a new Notification.kind requires adding it here too so
// clients see the new toggle in their preference UI; forgetting would
// silently hide the default (users get emailed, no way to opt out).
var knownKinds = []entnotificationpreference.Kind{
	entnotificationpreference.KindMention,
}

func (s *Service) GetPreferences(ctx context.Context, _ *connect.Request[huddlev1.NotificationServiceGetPreferencesRequest]) (*connect.Response[huddlev1.NotificationServiceGetPreferencesResponse], error) {
	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Read every explicit row the caller has once; fill in the defaults
	// for any kind they haven't touched. One query + a map lookup;
	// avoids N queries over knownKinds.
	rows, err := s.client.NotificationPreference.Query().
		Where(entnotificationpreference.UserIDEQ(caller.ID)).
		All(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load preferences: %w", err))
	}
	explicit := make(map[entnotificationpreference.Kind]*ent.NotificationPreference, len(rows))
	for _, p := range rows {
		explicit[p.Kind] = p
	}

	out := &huddlev1.NotificationServiceGetPreferencesResponse{
		Preferences: make([]*huddlev1.NotificationPreference, 0, len(knownKinds)),
	}
	for _, kind := range knownKinds {
		enabled := true // default (opt-out / industry norm)
		if p, ok := explicit[kind]; ok {
			enabled = p.EmailEnabled
		}
		out.Preferences = append(out.Preferences, &huddlev1.NotificationPreference{
			Kind:         string(kind),
			EmailEnabled: enabled,
		})
	}
	return connect.NewResponse(out), nil
}

func (s *Service) SetPreference(ctx context.Context, req *connect.Request[huddlev1.NotificationServiceSetPreferenceRequest]) (*connect.Response[huddlev1.NotificationServiceSetPreferenceResponse], error) {
	kind, err := parseKind(req.Msg.Kind)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	caller, err := s.resolver.Resolve(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// UPSERT on (user_id, kind). If a row exists, update email_enabled;
	// otherwise insert. ent's OnConflict + Update covers this in one
	// statement on Postgres (INSERT ... ON CONFLICT DO UPDATE) and
	// SQLite (ON CONFLICT DO UPDATE as of SQLite 3.24+).
	id, err := s.client.NotificationPreference.Create().
		SetUserID(caller.ID).
		SetKind(kind).
		SetEmailEnabled(req.Msg.EmailEnabled).
		OnConflictColumns(
			entnotificationpreference.FieldUserID,
			entnotificationpreference.FieldKind,
		).
		Update(func(u *ent.NotificationPreferenceUpsert) {
			u.SetEmailEnabled(req.Msg.EmailEnabled)
			u.UpdateUpdatedAt()
		}).
		ID(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("upsert preference: %w", err))
	}
	saved, err := s.client.NotificationPreference.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("reload preference: %w", err))
	}

	return connect.NewResponse(&huddlev1.NotificationServiceSetPreferenceResponse{
		Preference: &huddlev1.NotificationPreference{
			Kind:         string(saved.Kind),
			EmailEnabled: saved.EmailEnabled,
		},
	}), nil
}

// parseKind maps a wire string to the ent enum, rejecting anything that
// isn't a known kind. Keeps SetPreference from accepting "invented"
// kinds that would silently never map to a real Notification.
func parseKind(s string) (entnotificationpreference.Kind, error) {
	switch entnotificationpreference.Kind(s) {
	case entnotificationpreference.KindMention:
		return entnotificationpreference.KindMention, nil
	default:
		return "", errors.New("unknown notification kind")
	}
}
