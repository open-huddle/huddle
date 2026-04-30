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

	"entgo.io/ent/dialect"

	"github.com/open-huddle/huddle/apps/api/internal/audit"
	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/config"
	"github.com/open-huddle/huddle/apps/api/internal/database"
	"github.com/open-huddle/huddle/apps/api/internal/email"
	"github.com/open-huddle/huddle/apps/api/internal/events"
	"github.com/open-huddle/huddle/apps/api/internal/invitations"
	"github.com/open-huddle/huddle/apps/api/internal/notifications"
	"github.com/open-huddle/huddle/apps/api/internal/observability"
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

	// Initialize OpenTelemetry first so every dependency we open below
	// (DB, NATS, OpenSearch, OIDC verifier) has a working tracer/meter
	// from its first call. When observability.enabled=false the SDK
	// stays at no-op and the framework instrumentation below is free.
	obsShutdown, err := observability.Init(ctx, observability.Config{
		Enabled:        cfg.Observability.Enabled,
		OTLPEndpoint:   cfg.Observability.OTLPEndpoint,
		OTLPInsecure:   cfg.Observability.OTLPInsecure,
		ServiceName:    cfg.Observability.ServiceName,
		ServiceVersion: cfg.Observability.ServiceVersion,
	}, logger)
	if err != nil {
		return fmt.Errorf("init observability: %w", err)
	}
	defer func() {
		// Drain in-flight span/metric batches on the way out. Bound the
		// wait so a misbehaving collector can't stall shutdown forever.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer drainCancel()
		if err := obsShutdown(drainCtx); err != nil {
			logger.Warn("observability shutdown", "err", err)
		}
	}()

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

	// Pipeline metrics — Slice C of ADR-0019. End-to-end Send→Subscribe
	// latency is recorded from inside events.Subscribe; outbox-depth is an
	// observable gauge whose callback runs at metric collection time. Both
	// are no-ops when observability is disabled.
	pipelineMetrics, err := observability.NewPipelineMetrics()
	if err != nil {
		return fmt.Errorf("init pipeline metrics: %w", err)
	}
	depthRegistration, err := pipelineMetrics.RegisterOutboxDepth(observability.NewSQLOutboxDepthQuerier(db.SQL))
	if err != nil {
		return fmt.Errorf("register outbox depth gauge: %w", err)
	}
	defer func() {
		if err := depthRegistration.Unregister(); err != nil {
			logger.Warn("outbox depth gauge unregister", "err", err)
		}
	}()

	// Connect to NATS and ensure the JetStream stream exists. Failure here is
	// fatal at startup — operators should know up front that the realtime
	// path is broken rather than discover it on the first Subscribe call.
	natsCtx, ncancel := context.WithTimeout(ctx, 15*time.Second)
	bus, err := events.Open(natsCtx, cfg.Nats.URL, logger,
		events.WithPipelineMetrics(pipelineMetrics),
	)
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

	// Background workers: mirror outbox rows into the audit log, index
	// message projections into OpenSearch, deliver invitation + mention
	// notification emails, and GC fully-processed outbox rows once they
	// age past Outbox.Retention. NATS publish itself is driven by Debezium
	// Server reading the WAL — see ADR-0018. All workers share the
	// signal-cancellable ctx so they stop when SIGINT/SIGTERM fires;
	// rows they didn't reach stay durable in the DB and get picked up at
	// next startup.
	sender, err := buildEmailSender(cfg, logger)
	if err != nil {
		return fmt.Errorf("init email sender: %w", err)
	}

	// Shared worker metric instruments — one set, attribute-tagged per
	// worker, so dashboards can `sum by (worker)` over a single counter.
	workerMetrics, err := observability.NewWorkerMetrics()
	if err != nil {
		return fmt.Errorf("init worker metrics: %w", err)
	}

	auditConsumer := audit.NewConsumer(db.Ent, logger,
		audit.WithWorkerInstr(observability.NewWorkerInstr("audit", workerMetrics)),
	)
	searchIndexer := search.NewIndexer(db.Ent, searchClient, logger,
		search.WithIndexerDialect(dialect.Postgres),
		search.WithIndexerWorkerInstr(observability.NewWorkerInstr("search-indexer", workerMetrics)),
	)
	outboxGC := outbox.NewGC(db.Ent, logger,
		outbox.WithGCRetention(cfg.Outbox.Retention),
		outbox.WithGCWorkerInstr(observability.NewWorkerInstr("outbox-gc", workerMetrics)),
	)
	mailer := invitations.NewMailer(db.Ent, sender, logger,
		cfg.Email.FromAddress, cfg.Email.FromName, cfg.Invites.LinkBaseURL,
		invitations.WithDialect(dialect.Postgres),
		invitations.WithWorkerInstr(observability.NewWorkerInstr("invitations-mailer", workerMetrics)),
	)
	notificationsConsumer := notifications.NewConsumer(db.Ent, logger,
		notifications.WithDialect(dialect.Postgres),
		notifications.WithWorkerInstr(observability.NewWorkerInstr("notifications-consumer", workerMetrics)),
	)
	notificationsMailer := notifications.NewMailer(db.Ent, sender, logger,
		cfg.Email.FromAddress, cfg.Email.FromName, cfg.App.BaseURL,
		notifications.WithMailerDialect(dialect.Postgres),
		notifications.WithMailerWorkerInstr(observability.NewWorkerInstr("notifications-mailer", workerMetrics)),
	)

	go auditConsumer.Run(ctx)
	go searchIndexer.Run(ctx)
	go outboxGC.Run(ctx)
	go mailer.Run(ctx)
	go notificationsConsumer.Run(ctx)
	go notificationsMailer.Run(ctx)

	// Warn on the dev default for invites.secret — every real deployment
	// MUST override HUDDLE_INVITES_SECRET. Forging a token with the known
	// default would give anyone on the internet the ability to accept an
	// invite they didn't receive.
	if cfg.Invites.Secret == "" || cfg.Invites.Secret == "dev-only-invites-secret-change-in-prod" {
		logger.Warn("invites.secret is using the dev-only default; set HUDDLE_INVITES_SECRET before running outside local development")
	}

	srv := server.New(cfg, logger, db, verifier, bus, searchClient)

	httpSrv := &http.Server{
		Addr: cfg.Addr,
		// otelhttp.NewHandler wraps the chi router so every incoming
		// request gets a server span with the route, method, status,
		// and duration. Connect handlers and DB queries hang as child
		// spans off this root.
		Handler:           observability.HTTPHandler(srv.Handler()),
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

// buildEmailSender picks the right concrete email.Sender for the
// configured driver. Unknown / empty driver falls back to "log" so a
// fresh `make dev-up` starts cleanly without SMTP config; production
// must set driver=smtp alongside the relay credentials.
func buildEmailSender(cfg *config.Config, logger *slog.Logger) (email.Sender, error) {
	switch cfg.Email.Driver {
	case "smtp":
		return email.NewSMTPSender(email.SMTPConfig{
			Host:     cfg.Email.SMTP.Host,
			Port:     cfg.Email.SMTP.Port,
			Username: cfg.Email.SMTP.Username,
			Password: cfg.Email.SMTP.Password,
			StartTLS: cfg.Email.SMTP.StartTLS,
		})
	case "log", "":
		return email.NewLogSender(logger), nil
	default:
		logger.Warn("unknown email.driver; falling back to log", "driver", cfg.Email.Driver)
		return email.NewLogSender(logger), nil
	}
}
