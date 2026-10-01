package config

import (
	"strings"
	"testing"
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
