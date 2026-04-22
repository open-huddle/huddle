package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-huddle/huddle/apps/api/internal/audit"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/config"
	"github.com/open-huddle/huddle/apps/api/internal/database"
	"github.com/open-huddle/huddle/apps/api/internal/events"
	"github.com/open-huddle/huddle/apps/api/internal/outbox"
	"github.com/open-huddle/huddle/apps/api/internal/search"
	"github.com/open-huddle/huddle/apps/api/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run wraps the startup/shutdown sequence so all defers (signal stop, DB close,
// context cancels) run before os.Exit — os.Exit skips them if called inline.
func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	db, err := database.Open(connectCtx, database.Config{
		URL:             cfg.Database.URL,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
		ConnMaxIdleTime: cfg.Database.ConnMaxIdleTime,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close database", "err", err)
		}
	}()

	// Fetch the OIDC discovery document and JWKS once at startup. The verifier
	// keeps the JWKS fresh in the background, so Keycloak key rotations are
	// picked up without a restart.
	verifyCtx, vcancel := context.WithTimeout(ctx, 15*time.Second)
	verifier, err := auth.NewVerifier(verifyCtx, cfg.Auth.IssuerURL, cfg.Auth.Audience)
	vcancel()
	if err != nil {
		return fmt.Errorf("init oidc verifier: %w", err)
	}

	// Connect to NATS and ensure the JetStream stream exists. Failure here is
	// fatal at startup — operators should know up front that the realtime
	// path is broken rather than discover it on the first Subscribe call.
	natsCtx, ncancel := context.WithTimeout(ctx, 15*time.Second)
	bus, err := events.Open(natsCtx, cfg.Nats.URL, logger)
	ncancel()
	if err != nil {
		return fmt.Errorf("init nats: %w", err)
	}
	defer bus.Close()

	// OpenSearch: same fail-fast story as NATS — search being broken at
	// startup should surface immediately, not on the first query. EnsureIndex
	// creates the concrete index + alias idempotently.
	searchClient, err := search.NewOpenSearch(cfg.OpenSearch.URL, cfg.OpenSearch.MessagesIndex)
	if err != nil {
		return fmt.Errorf("init opensearch: %w", err)
	}
	ensureCtx, ecancel := context.WithTimeout(ctx, 15*time.Second)
	if err := searchClient.EnsureIndex(ensureCtx); err != nil {
		ecancel()
		return fmt.Errorf("opensearch ensure index: %w", err)
	}
	ecancel()

	// Background workers: drain the transactional outbox to NATS, mirror
	// outbox rows into the audit log, index message projections into
	// OpenSearch, and GC fully-processed outbox rows once they age past
	// Outbox.Retention. All share the signal-cancellable ctx so they stop
	// when SIGINT/SIGTERM fires — rows they didn't reach stay durable in
	// the DB and get picked up at next startup.
	outboxPublisher := outbox.NewPublisher(db.Ent, bus, logger)
	auditConsumer := audit.NewConsumer(db.Ent, logger)
	searchIndexer := search.NewIndexer(db.Ent, searchClient, logger)
	outboxGC := outbox.NewGC(db.Ent, logger, outbox.WithGCRetention(cfg.Outbox.Retention))
	go outboxPublisher.Run(ctx)
	go auditConsumer.Run(ctx)
	go searchIndexer.Run(ctx)
	go outboxGC.Run(ctx)

	srv := server.New(cfg, logger, db, verifier, bus, searchClient)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer scancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
