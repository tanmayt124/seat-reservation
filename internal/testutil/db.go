// Package testutil holds helpers shared by tests. It is imported only from
// _test.go files.
package testutil

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaSeq atomic.Int64

// Pool connects to TEST_DATABASE_URL inside a throwaway schema, so tests never
// touch real tables and can run in parallel. Skips when the variable is unset.
// The schema is empty; call store.Migrate if the test needs tables.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("t_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))

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
	pc.MaxConns = 32
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

func QuietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
