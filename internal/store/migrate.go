package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is an arbitrary constant. Holding this advisory lock while
// migrating means two app instances booting together cannot both apply the
// same migration.
const migrationLockID int64 = 0x5EA7_0001

// Migrate applies every *.sql file in fsys that is not yet recorded in
// schema_migrations, in filename order, each inside its own transaction.
// Running it again is a no-op.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) error {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migrations: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context so the lock is released even if ctx was cancelled.
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := 0
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")

		var exists bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists {
			continue
		}

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}

		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// No arguments, so pgx uses the simple protocol and a file can hold
			// several statements.
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		log.Info("migration applied", "version", version)
		applied++
	}

	log.Info("migrations up to date", "applied_now", applied, "total", len(names))
	return nil
}
