// Package config loads the ledger server settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGRPCAddr           = "127.0.0.1:9091"
	defaultCheckpointInterval = time.Hour
	defaultCheckpointGrace    = time.Minute
)

// Config is the ledger server configuration.
type Config struct {
	// GRPCAddr is the internal listen address. It defaults to loopback; in a cluster set it to
	// 0.0.0.0:9091 behind a ClusterIP Service that the gateway never routes to.
	GRPCAddr string

	// DatabaseURL is the PostgreSQL connection string (a URL or key=value form). Put the password
	// in DatabasePasswordFile instead of in the URL, so it need not sit in the environment.
	DatabaseURL string

	// DatabasePassword is the contents of the password file, if one was given.
	DatabasePassword string

	// Migrate applies pending database migrations at startup.
	Migrate bool

	// CheckpointInterval is how often the checkpointer looks for finished periods. Zero turns the
	// background checkpointer off (balances stay correct, they just sum more entries).
	CheckpointInterval time.Duration

	// CheckpointGrace is how long after a period ends before it is checkpointed.
	CheckpointGrace time.Duration
}

// Load reads the configuration through getenv (pass os.Getenv outside of tests).
//
//	LEDGER_GRPC_ADDR                gRPC listen address           (default 127.0.0.1:9091)
//	LEDGER_DATABASE_URL             PostgreSQL connection string  (required)
//	LEDGER_DATABASE_PASSWORD_FILE   file holding the password     (optional)
//	LEDGER_MIGRATE                  apply migrations at startup   (default true)
//	LEDGER_CHECKPOINT_INTERVAL      how often to checkpoint       (default 1h; 0 disables)
//	LEDGER_CHECKPOINT_GRACE         wait after a period ends      (default 1m)
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		GRPCAddr:           valueOr(getenv("LEDGER_GRPC_ADDR"), defaultGRPCAddr),
		DatabaseURL:        strings.TrimSpace(getenv("LEDGER_DATABASE_URL")),
		Migrate:            true,
		CheckpointInterval: defaultCheckpointInterval,
		CheckpointGrace:    defaultCheckpointGrace,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("LEDGER_DATABASE_URL is required")
	}

	if raw := getenv("LEDGER_MIGRATE"); raw != "" {
		migrate, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("LEDGER_MIGRATE must be true or false, got %q", raw)
		}
		cfg.Migrate = migrate
	}

	var err error
	if cfg.CheckpointInterval, err = parseDuration(getenv, "LEDGER_CHECKPOINT_INTERVAL", defaultCheckpointInterval); err != nil {
		return Config{}, err
	}
	if cfg.CheckpointGrace, err = parseDuration(getenv, "LEDGER_CHECKPOINT_GRACE", defaultCheckpointGrace); err != nil {
		return Config{}, err
	}

	if file := getenv("LEDGER_DATABASE_PASSWORD_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return Config{}, fmt.Errorf("read LEDGER_DATABASE_PASSWORD_FILE: %w", err)
		}
		cfg.DatabasePassword = strings.TrimSpace(string(data))
	}

	return cfg, nil
}

// parseDuration reads a Go duration that may be zero but not negative.
func parseDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration such as 1h or 90s, got %q", name, raw)
	}

	return value, nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
