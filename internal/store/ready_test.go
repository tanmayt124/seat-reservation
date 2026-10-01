package store

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tanmayt124/seat-reservation/internal/testutil"
	"github.com/tanmayt124/seat-reservation/migrations"
)

func TestReadyCheck(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	check := ReadyCheck(pool, migrations.FS)

	// Before migrations: schema_migrations does not exist yet.
	if err := check(ctx); err == nil {
		t.Fatal("expected not ready before migrations")
	}
	if err := Migrate(ctx, pool, migrations.FS, testutil.QuietLogger()); err != nil {
		t.Fatal(err)
	}
	if err := check(ctx); err != nil {
		t.Fatalf("expected ready, got %v", err)
	}

	// A binary that ships a newer migration than the DB has is not ready.
	newer := fstest.MapFS{}
	for _, n := range []string{"0001_init.sql", "0002_money_and_limits.sql", "0003_future.sql"} {
		newer[n] = &fstest.MapFile{Data: []byte("--")}
	}
	if err := ReadyCheck(pool, newer)(ctx); err == nil || !strings.Contains(err.Error(), "migrations_pending") {
		t.Fatalf("expected migrations_pending, got %v", err)
	}

	// Database gone: fails closed.
	pool.Close()
	if err := check(ctx); err == nil {
		t.Fatal("expected not ready with the pool closed")
	}
}
