package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"

	"github.com/open-huddle/huddle/apps/api/ent"
	"github.com/open-huddle/huddle/apps/api/ent/user"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
)

// Compile-time check that Service satisfies the generated handler interface.
var _ huddlev1connect.IdentityServiceHandler = (*Service)(nil)

// Service implements IdentityService. It owns the user-upsert-on-first-login
// flow: the first authenticated call for a given OIDC subject creates the
// row; subsequent calls refresh email and display name from the latest
// token.
type Service struct {
	client *ent.Client
	logger *slog.Logger
}

func New(client *ent.Client, logger *slog.Logger) *Service {
	return &Service{client: client, logger: logger}
}

func (s *Service) WhoAmI(ctx context.Context, req *connect.Request[huddlev1.WhoAmIRequest]) (*connect.Response[huddlev1.WhoAmIResponse], error) {
	claims, ok := auth.ClaimsFrom(ctx)
	if !ok {
		// Reaching this branch means the route was not wrapped by the auth
		// interceptor — a wiring bug, not a runtime auth failure.
		return nil, connect.NewError(connect.CodeUnauthenticated, auth.ErrNoClaims)
	}

	u, err := s.upsert(ctx, claims)
	if err != nil {
		s.logger.Error("upsert user", "err", err, "subject", claims.Subject)
		return nil, connect.NewError(connect.CodeInternal, errors.New("upsert user"))
	}

	return connect.NewResponse(&huddlev1.WhoAmIResponse{
		UserId:      u.ID.String(),
		Subject:     u.Subject,
		Email:       u.Email,
		DisplayName: u.DisplayName,
	}), nil
}

// upsert creates or refreshes the users row for the current token. Subject is
// the conflict key (unique), so the same OIDC identity always maps to the
// same row even if email or display name change upstream.
func (s *Service) upsert(ctx context.Context, c auth.Claims) (*ent.User, error) {
	id, err := s.client.User.Create().
		SetSubject(c.Subject).
		SetEmail(c.Email).
		SetDisplayName(c.DisplayName()).
		OnConflictColumns(user.FieldSubject).
		Update(func(u *ent.UserUpsert) {
			u.SetEmail(c.Email)
			u.SetDisplayName(c.DisplayName())
			u.UpdateUpdatedAt()
		}).
		ID(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert: %w", err)
	}
	return s.client.User.Get(ctx, id)
}
