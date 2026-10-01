package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tanmayt124/seat-reservation/internal/testutil"
	"github.com/tanmayt124/seat-reservation/migrations"
)

func TestMigrateIsRepeatable(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, pool, migrations.FS, testutil.QuietLogger()); err != nil {
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
	pool := testutil.Pool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool, migrations.FS, testutil.QuietLogger()); err != nil {
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
