// Package config reads all runtime settings from environment variables.
// Nothing is read from files; .env is only a convenience for `make run`.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
	DBMaxConns  int32
	// AdmissionLimit caps in-flight write requests; 0 turns the limiter off.
	// Off by default: shedding a hot-seat loser with 429 would break the
	// "everyone else gets 409" rule, so bursts queue on the DB pool instead.
	AdmissionLimit  int
	AdmissionWait   time.Duration
	PerUserLimit    int
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// DBAcquireTimeout is how long a request waits for a pool connection
	// before answering 429. Long on purpose: bursts queue rather than shed.
	DBAcquireTimeout time.Duration
	// WriteTimeout is the HTTP server's write deadline, derived from
	// DBAcquireTimeout so raising the queue wait never turns a slow answer
	// into a dropped connection. See writeTimeoutFor.
	WriteTimeout time.Duration
	// EnableTokenEndpoint exposes POST /auth/token so the burst script can mint
	// test tokens. It is a test helper and is off unless set explicitly.
	EnableTokenEndpoint bool
}

// Load reads the environment and returns every problem at once, so a bad
// deploy fails with one clear message instead of a fix-one-rerun loop.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		Port:                getenv("PORT", "8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		JWTSecret:           os.Getenv("JWT_SECRET"),
		EnableTokenEndpoint: getenv("ENABLE_TOKEN_ENDPOINT", "false") == "true",
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if len(cfg.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET is required and must be at least 32 characters"))
	}

	maxConns, err := intEnv("DB_MAX_CONNS", 16, 2, 200)
	errs = appendErr(errs, err)
	cfg.DBMaxConns = int32(maxConns)

	cfg.AdmissionLimit, err = intEnv("ADMISSION_LIMIT", 0, 0, 100000)
	errs = appendErr(errs, err)

	cfg.AdmissionWait, err = durationEnv("ADMISSION_WAIT", 10*time.Second)
	errs = appendErr(errs, err)

	cfg.PerUserLimit, err = intEnv("PER_USER_LIMIT", 4, 1, 100)
	errs = appendErr(errs, err)

	cfg.ShutdownTimeout, err = durationEnv("SHUTDOWN_TIMEOUT", 20*time.Second)
	errs = appendErr(errs, err)

	cfg.DBAcquireTimeout, err = durationEnv("DB_ACQUIRE_TIMEOUT", 10*time.Second)
	errs = appendErr(errs, err)
	if err == nil && cfg.DBAcquireTimeout > maxAcquireTimeout {
		errs = append(errs, fmt.Errorf("DB_ACQUIRE_TIMEOUT must be at most %s, got %s", maxAcquireTimeout, cfg.DBAcquireTimeout))
	}
	cfg.WriteTimeout = writeTimeoutFor(cfg.DBAcquireTimeout)

	cfg.LogLevel, err = levelEnv("LOG_LEVEL", slog.LevelInfo)
	errs = appendErr(errs, err)

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// maxAcquireTimeout keeps the derived write deadline under two and a half
// minutes; a request queued longer than that is better answered with 429.
const maxAcquireTimeout = 60 * time.Second

// writeTimeoutFor returns the HTTP write deadline for a given pool wait. A
// reservation can wait for a connection twice (the lock-free pre-check, then
// the transaction) and then spend up to a few seconds in the transaction
// (lock_timeout 3s, statement_timeout 5s), so the deadline is twice the wait
// plus 20s of headroom, and never below the old fixed 40s.
func writeTimeoutFor(acquire time.Duration) time.Duration {
	return max(40*time.Second, 2*acquire+20*time.Second)
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func intEnv(key string, def, min, max int) (int, error) {
	raw := getenv(key, strconv.Itoa(def))
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	if n < min || n > max {
		return def, fmt.Errorf("%s must be between %d and %d, got %d", key, min, max, n)
	}
	return n, nil
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	raw := getenv(key, def.String())
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def, fmt.Errorf("%s must be a positive duration like 20s, got %q", key, raw)
	}
	return d, nil
}

func levelEnv(key string, def slog.Level) (slog.Level, error) {
	raw := strings.ToLower(getenv(key, "info"))
	switch raw {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return def, fmt.Errorf("%s must be debug, info, warn or error, got %q", key, raw)
}

func appendErr(errs []error, err error) []error {
	if err != nil {
		return append(errs, err)
	}
	return errs
}
