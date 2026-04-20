package server

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/open-huddle/huddle/apps/api/internal/config"
	"github.com/open-huddle/huddle/apps/api/internal/services/health"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	router *chi.Mux
}

func New(cfg *config.Config, logger *slog.Logger) *Server {
	s := &Server{cfg: cfg, logger: logger, router: chi.NewRouter()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.router.Use(middleware.RequestID)
	s.router.Use(middleware.RealIP)
	s.router.Use(middleware.Recoverer)

	// Plain HTTP probes (for k8s, not part of the RPC surface).
	s.router.Get("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.router.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Connect services.
	healthSvc := health.New(s.cfg.Version)
	path, handler := huddlev1connect.NewHealthServiceHandler(healthSvc)
	s.router.Mount(path, handler)
}

// Handler returns an http.Handler that supports HTTP/2 cleartext — required for
// gRPC clients that don't negotiate TLS (e.g. local dev, in-cluster mesh).
func (s *Server) Handler() http.Handler {
	return h2c.NewHandler(s.router, &http2.Server{})
}
