package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresDatabaseAndSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", "short")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"DATABASE_URL", "JWT_SECRET"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %s", msg, want)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", strings.Repeat("s", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "8080" || cfg.PerUserLimit != 4 || cfg.DBMaxConns != 16 || cfg.EnableTokenEndpoint || cfg.AdmissionLimit != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadRejectsOutOfRange(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", strings.Repeat("s", 32))
	t.Setenv("PER_USER_LIMIT", "0")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PER_USER_LIMIT") {
		t.Fatalf("expected PER_USER_LIMIT error, got %v", err)
	}
}

func TestWriteTimeoutFollowsAcquireTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", strings.Repeat("s", 32))

	cases := map[string]time.Duration{
		"":    40 * time.Second, // default 10s keeps the old 40s
		"5s":  40 * time.Second, // never below 40s
		"30s": 80 * time.Second, // 2 x 30s + 20s
		"60s": 140 * time.Second,
	}
	for acquire, want := range cases {
		t.Setenv("DB_ACQUIRE_TIMEOUT", acquire)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("DB_ACQUIRE_TIMEOUT=%q: %v", acquire, err)
		}
		if cfg.WriteTimeout != want {
			t.Errorf("DB_ACQUIRE_TIMEOUT=%q: write timeout %s, want %s", acquire, cfg.WriteTimeout, want)
		}
		// Worst case for one reservation: two pool waits plus the
		// transaction's own lock and statement limits.
		if worst := 2*cfg.DBAcquireTimeout + 8*time.Second; cfg.WriteTimeout <= worst {
			t.Errorf("DB_ACQUIRE_TIMEOUT=%q: write timeout %s does not cover worst case %s", acquire, cfg.WriteTimeout, worst)
		}
	}

	t.Setenv("DB_ACQUIRE_TIMEOUT", "61s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DB_ACQUIRE_TIMEOUT") {
		t.Fatalf("expected DB_ACQUIRE_TIMEOUT bound error, got %v", err)
	}
}
