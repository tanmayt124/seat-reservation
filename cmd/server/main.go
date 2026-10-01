// Command server runs the seat reservation HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tanmayt124/seat-reservation/internal/auth"
	"github.com/tanmayt124/seat-reservation/internal/config"
	"github.com/tanmayt124/seat-reservation/internal/httpapi"
	"github.com/tanmayt124/seat-reservation/internal/metrics"
	"github.com/tanmayt124/seat-reservation/internal/reserve"
	"github.com/tanmayt124/seat-reservation/internal/store"
	"github.com/tanmayt124/seat-reservation/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, store.PoolConfig{
		URL:      cfg.DatabaseURL,
		MaxConns: cfg.DBMaxConns,
	}, logger)
	if err != nil {
		return err
	}
	// Closed last, after the HTTP server has drained in-flight requests.
	defer pool.Close()

	if err := store.Migrate(ctx, pool, migrations.FS, logger); err != nil {
		return err
	}
	if cfg.EnableTokenEndpoint {
		logger.Warn("POST /auth/token is enabled: test helper, anyone can mint tokens")
	}

	// Readiness fails as soon as shutdown starts, so the platform stops
	// routing new traffic here while in-flight requests drain.
	var draining atomic.Bool
	dbReady := store.ReadyCheck(pool, migrations.FS)
	ready := func(ctx context.Context) error {
		if draining.Load() {
			return errors.New("shutting_down")
		}
		return dbReady(ctx)
	}

	reserveSvc := reserve.NewService(pool, logger)
	reserveSvc.SetAcquireTimeout(cfg.DBAcquireTimeout)

	srv := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Logger:              logger,
			Auth:                auth.New(cfg.JWTSecret),
			Shows:               store.NewShows(pool),
			Reserve:             reserveSvc,
			EnableTokenEndpoint: cfg.EnableTokenEndpoint,
			DefaultPerUserLimit: cfg.PerUserLimit,
			Ready:               ready,
			Metrics:             metrics.New(pool, logger),
			AdmissionLimit:      cfg.AdmissionLimit,
			AdmissionWait:       cfg.AdmissionWait,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      40 * time.Second, // above the worst-case queue wait (2 x DB_ACQUIRE_TIMEOUT)
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	draining.Store(true)
	logger.Info("shutdown signal received, draining", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("shutdown complete")
	return nil
}
