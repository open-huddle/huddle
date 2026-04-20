package identity

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"

	"github.com/open-huddle/huddle/apps/api/internal/principal"
)

var _ huddlev1connect.IdentityServiceHandler = (*Service)(nil)

// Service implements IdentityService. The user-upsert side effect lives in
// internal/principal so every authenticated handler shares the same
// "materialize the calling user" path; this handler is now a thin wrapper.
type Service struct {
	resolver *principal.Resolver
	logger   *slog.Logger
}

func New(resolver *principal.Resolver, logger *slog.Logger) *Service {
	return &Service{resolver: resolver, logger: logger}
}

func (s *Service) WhoAmI(ctx context.Context, _ *connect.Request[huddlev1.WhoAmIRequest]) (*connect.Response[huddlev1.WhoAmIResponse], error) {
	u, err := s.resolver.Resolve(ctx)
	if err != nil {
		s.logger.Error("resolve principal", "err", err)
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("resolve principal"))
	}
	return connect.NewResponse(&huddlev1.WhoAmIResponse{
		UserId:      u.ID.String(),
		Subject:     u.Subject,
		Email:       u.Email,
		DisplayName: u.DisplayName,
	}), nil
}
