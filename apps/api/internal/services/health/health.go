package health

import (
	"context"

	"connectrpc.com/connect"

	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

type Service struct {
	version string
}

var _ huddlev1connect.HealthServiceHandler = (*Service)(nil)

func New(version string) *Service {
	return &Service{version: version}
}

func (s *Service) Check(_ context.Context, _ *connect.Request[huddlev1.CheckRequest]) (*connect.Response[huddlev1.CheckResponse], error) {
	return connect.NewResponse(&huddlev1.CheckResponse{
		Status:  huddlev1.CheckResponse_STATUS_SERVING,
		Version: s.version,
	}), nil
}
