package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tanmayt124/seat-reservation/migrations"
)

// testPool connects to TEST_DATABASE_URL inside a throwaway schema, so tests
// never touch real tables and can run in parallel. Skips when unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}

	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return pool
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMigrateIsRepeatable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, pool, migrations.FS, quietLogger()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations rows = %d, want 1", n)
	}
}

func TestSchemaRejectsImpossibleSeatStates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool, migrations.FS, quietLogger()); err != nil {
		t.Fatal(err)
	}

	var showID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO shows (name, total_seats) VALUES ('test', 2) RETURNING id",
	).Scan(&showID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO seats (show_id, label) VALUES ($1, 'A1'), ($1, 'A2')", showID,
	); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		sql      string
		wantCode string
	}{
		{"duplicate label", "INSERT INTO seats (show_id, label) VALUES ($1, 'A1')", "23505"},
		{"confirmed without holder", "UPDATE seats SET status = 'confirmed' WHERE show_id = $1 AND label = 'A1'", "23514"},
		{"available with holder", "UPDATE seats SET user_id = 'u1' WHERE show_id = $1 AND label = 'A2'", "23514"},
		{"unknown status", "UPDATE seats SET status = 'sold' WHERE show_id = $1 AND label = 'A1'", "23514"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, c.sql, showID)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != c.wantCode {
				t.Fatalf("got %v, want SQLSTATE %s", err, c.wantCode)
			}
		})
	}
}
