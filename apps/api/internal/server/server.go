package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/open-huddle/huddle/apps/api/internal/config"
	"github.com/open-huddle/huddle/apps/api/internal/database"
	"github.com/open-huddle/huddle/apps/api/internal/services/health"
	"github.com/open-huddle/huddle/gen/go/huddle/v1/huddlev1connect"
)

type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	db     *database.DB
	router *chi.Mux
}

func New(cfg *config.Config, logger *slog.Logger, db *database.DB) *Server {
	s := &Server{cfg: cfg, logger: logger, db: db, router: chi.NewRouter()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.router.Use(middleware.RequestID)
	s.router.Use(middleware.RealIP)
	s.router.Use(middleware.Recoverer)

	// Plain HTTP probes (for k8s, not part of the RPC surface).
	s.router.Get("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.router.Get("/readyz", s.readyz)

	// Connect services.
	healthSvc := health.New(s.cfg.Version)
	path, handler := huddlev1connect.NewHealthServiceHandler(healthSvc)
	s.router.Mount(path, handler)
}

// readyz reports ready only when the API can reach its dependencies. Kubelet
// will stop routing traffic to this pod while it returns 503.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.SQL.PingContext(ctx); err != nil {
		s.logger.Warn("readyz: db unreachable", "err", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Handler returns an http.Handler that supports HTTP/2 cleartext — required for
// gRPC clients that don't negotiate TLS (e.g. local dev, in-cluster mesh).
func (s *Server) Handler() http.Handler {
	return h2c.NewHandler(s.router, &http2.Server{})
}
