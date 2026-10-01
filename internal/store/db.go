// Package store owns the Postgres connection pool and schema migrations.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PoolConfig struct {
	URL      string
	MaxConns int32
	// ConnectTimeout bounds how long startup waits for the database to accept
	// connections. Compose orders startup with a healthcheck, but hosted
	// platforms do not, so the app retries instead of crash-looping.
	ConnectTimeout time.Duration
}

func NewPool(ctx context.Context, c PoolConfig, log *slog.Logger) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pc.MaxConns = c.MaxConns
	pc.MinConns = 2
	pc.MaxConnLifetime = 30 * time.Minute
	pc.MaxConnIdleTime = 5 * time.Minute
	pc.HealthCheckPeriod = 30 * time.Second
	pc.ConnConfig.ConnectTimeout = 5 * time.Second
	pc.ConnConfig.RuntimeParams["application_name"] = "seat-reservation"
	// Safety nets for every session, set after connect rather than as startup
	// parameters so a pooler in front of Postgres does not reject them. The
	// reserve transaction sets tighter limits of its own with SET LOCAL.
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET statement_timeout = '10s'; SET idle_in_transaction_session_timeout = '15s'")
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	timeout := c.ConnectTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			log.Info("database connected", "max_conns", c.MaxConns, "attempts", attempt)
			return pool, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			pool.Close()
			return nil, fmt.Errorf("database not reachable after %s: %w", timeout, err)
		}
		log.Warn("database not ready, retrying", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
