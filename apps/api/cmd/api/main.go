package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-huddle/huddle/apps/api/internal/auth"
	"github.com/open-huddle/huddle/apps/api/internal/config"
	"github.com/open-huddle/huddle/apps/api/internal/database"
	"github.com/open-huddle/huddle/apps/api/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
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
		logger.Error("connect database", "err", err)
		os.Exit(1)
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
		logger.Error("init oidc verifier", "err", err)
		os.Exit(1)
	}

	srv := server.New(cfg, logger, db, verifier)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("api listening", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}
}
