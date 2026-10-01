package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReadyCheck returns a readiness probe: the database answers a query and
// every embedded migration is applied. Errors are short and safe to show on
// a public endpoint.
func ReadyCheck(pool *pgxpool.Pool, migrations fs.FS) func(context.Context) error {
	names, _ := fs.Glob(migrations, "*.sql")
	want := len(names)
	return func(ctx context.Context) error {
		var applied int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return errors.New("database_timeout")
			}
			return errors.New("database_unreachable")
		}
		if applied < want {
			return fmt.Errorf("migrations_pending: %d of %d applied", applied, want)
		}
		return nil
	}
}
