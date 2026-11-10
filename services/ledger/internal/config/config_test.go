package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/config"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

const databaseURL = "postgres://ledger@db:5432/ledger"

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"LEDGER_DATABASE_URL": databaseURL}))

	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GRPCAddr != "127.0.0.1:9091" {
		t.Errorf("GRPCAddr = %q, want loopback 127.0.0.1:9091", cfg.GRPCAddr)
	}
	if !cfg.Migrate || cfg.CheckpointInterval != time.Hour || cfg.CheckpointGrace != time.Minute {
		t.Errorf("cfg = %+v, want migrations on, hourly checkpoints, a one minute grace", cfg)
	}
	if cfg.DatabaseURL != databaseURL || cfg.DatabasePassword != "" {
		t.Errorf("database settings = %q / %q", cfg.DatabaseURL, cfg.DatabasePassword)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"LEDGER_DATABASE_URL":        databaseURL,
		"LEDGER_GRPC_ADDR":           "0.0.0.0:7000",
		"LEDGER_MIGRATE":             "false",
		"LEDGER_CHECKPOINT_INTERVAL": "15m",
		"LEDGER_CHECKPOINT_GRACE":    "90s",
	}))

	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GRPCAddr != "0.0.0.0:7000" || cfg.Migrate || cfg.CheckpointInterval != 15*time.Minute || cfg.CheckpointGrace != 90*time.Second {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadAllowsTurningTheCheckpointerOff(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"LEDGER_DATABASE_URL": databaseURL, "LEDGER_CHECKPOINT_INTERVAL": "0"}))

	if err != nil || cfg.CheckpointInterval != 0 {
		t.Errorf("interval = %v, err = %v; want 0 (off)", cfg.CheckpointInterval, err)
	}
}

func TestLoadReadsThePasswordFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(env(map[string]string{"LEDGER_DATABASE_URL": databaseURL, "LEDGER_DATABASE_PASSWORD_FILE": path}))

	if err != nil || cfg.DatabasePassword != "s3cret" {
		t.Errorf("password = %q, err = %v; want s3cret without the newline", cfg.DatabasePassword, err)
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	tests := map[string]struct {
		env     map[string]string
		message string
	}{
		"no database url":            {map[string]string{}, "LEDGER_DATABASE_URL is required"},
		"blank database url":         {map[string]string{"LEDGER_DATABASE_URL": "   "}, "LEDGER_DATABASE_URL is required"},
		"migrate is not a boolean":   {map[string]string{"LEDGER_DATABASE_URL": databaseURL, "LEDGER_MIGRATE": "maybe"}, "LEDGER_MIGRATE"},
		"interval is not a duration": {map[string]string{"LEDGER_DATABASE_URL": databaseURL, "LEDGER_CHECKPOINT_INTERVAL": "hourly"}, "LEDGER_CHECKPOINT_INTERVAL"},
		"interval is negative":       {map[string]string{"LEDGER_DATABASE_URL": databaseURL, "LEDGER_CHECKPOINT_INTERVAL": "-1h"}, "LEDGER_CHECKPOINT_INTERVAL"},
		"grace is negative":          {map[string]string{"LEDGER_DATABASE_URL": databaseURL, "LEDGER_CHECKPOINT_GRACE": "-5s"}, "LEDGER_CHECKPOINT_GRACE"},
		"missing password file":      {map[string]string{"LEDGER_DATABASE_URL": databaseURL, "LEDGER_DATABASE_PASSWORD_FILE": "/nope/password"}, "LEDGER_DATABASE_PASSWORD_FILE"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(env(tc.env))

			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %v, want it to mention %q", err, tc.message)
			}
		})
	}
}
