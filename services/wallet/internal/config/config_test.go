package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/config"
)

func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

// minimal is the least a valid configuration needs.
func minimal() map[string]string {
	return map[string]string{
		"WALLET_DATABASE_URL":    "postgres://u@localhost/wallet",
		"WALLET_OTP_STATIC_CODE": "246810",
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := config.Load(env(minimal()))
	if err != nil {
		t.Fatal(err)
	}

	want := config.Config{
		HTTPAddr: "127.0.0.1:8081", GRPCAddr: "127.0.0.1:9092", DatabaseURL: "postgres://u@localhost/wallet", Migrate: true,
		LedgerAddr: "127.0.0.1:9091", LedgerTimeout: 3 * time.Second, AuthAddr: "127.0.0.1:9090", AuthTimeout: 2 * time.Second,
		OTPMode: "static", OTPStaticCode: "246810", FundingMode: "disabled",
		MaxAccounts: 10, MaxHoldTTL: 168 * time.Hour, ConfirmTTL: 10 * time.Minute, TimeoutWorkdays: 5,
		RecoveryInterval: 15 * time.Second, RecoveryMinAge: 30 * time.Second,
	}
	if cfg != want {
		t.Errorf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestEverythingCanBeSetFromTheEnvironment(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"WALLET_HTTP_ADDR": "0.0.0.0:80", "WALLET_GRPC_ADDR": "0.0.0.0:90", "WALLET_DATABASE_URL": "postgres://x/y",
		"WALLET_MIGRATE": "false", "WALLET_LEDGER_ADDR": "ledger:1", "WALLET_LEDGER_TIMEOUT": "5s",
		"WALLET_AUTH_ADDR": "auth:2", "WALLET_AUTH_TIMEOUT": "1s", "WALLET_OTP_MODE": "DISABLED",
		"WALLET_FUNDING_MODE": "Simulated", "WALLET_MAX_ACCOUNTS": "3", "WALLET_MAX_HOLD_TTL": "2h",
		"WALLET_CONFIRM_TTL": "90s", "WALLET_OPERATION_TIMEOUT_WORKDAYS": "0",
		"WALLET_RECOVERY_INTERVAL": "0", "WALLET_RECOVERY_MIN_AGE": "0s",
	}))
	if err != nil {
		t.Fatal(err)
	}

	want := config.Config{
		HTTPAddr: "0.0.0.0:80", GRPCAddr: "0.0.0.0:90", DatabaseURL: "postgres://x/y", Migrate: false,
		LedgerAddr: "ledger:1", LedgerTimeout: 5 * time.Second, AuthAddr: "auth:2", AuthTimeout: time.Second,
		OTPMode: "disabled", FundingMode: "simulated",
		MaxAccounts: 3, MaxHoldTTL: 2 * time.Hour, ConfirmTTL: 90 * time.Second, TimeoutWorkdays: 0,
		RecoveryInterval: 0, RecoveryMinAge: 0,
	}
	if cfg != want {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
}

func TestInvalidSettingsAreRefused(t *testing.T) {
	tests := map[string]struct {
		change map[string]string
		want   string
	}{
		"no database":                {map[string]string{"WALLET_DATABASE_URL": " "}, "WALLET_DATABASE_URL is required"},
		"static otp without a code":  {map[string]string{"WALLET_OTP_STATIC_CODE": ""}, "WALLET_OTP_STATIC_CODE is required"},
		"unknown otp mode":           {map[string]string{"WALLET_OTP_MODE": "sms"}, "WALLET_OTP_MODE"},
		"unknown funding mode":       {map[string]string{"WALLET_FUNDING_MODE": "real"}, "WALLET_FUNDING_MODE"},
		"bad migrate":                {map[string]string{"WALLET_MIGRATE": "maybe"}, "WALLET_MIGRATE"},
		"bad ledger timeout":         {map[string]string{"WALLET_LEDGER_TIMEOUT": "fast"}, "WALLET_LEDGER_TIMEOUT"},
		"zero ledger timeout":        {map[string]string{"WALLET_LEDGER_TIMEOUT": "0s"}, "WALLET_LEDGER_TIMEOUT"},
		"negative auth timeout":      {map[string]string{"WALLET_AUTH_TIMEOUT": "-1s"}, "WALLET_AUTH_TIMEOUT"},
		"zero confirm ttl":           {map[string]string{"WALLET_CONFIRM_TTL": "0"}, "WALLET_CONFIRM_TTL"},
		"zero hold ttl":              {map[string]string{"WALLET_MAX_HOLD_TTL": "0s"}, "WALLET_MAX_HOLD_TTL"},
		"negative recovery interval": {map[string]string{"WALLET_RECOVERY_INTERVAL": "-5s"}, "WALLET_RECOVERY_INTERVAL"},
		"no accounts allowed":        {map[string]string{"WALLET_MAX_ACCOUNTS": "0"}, "WALLET_MAX_ACCOUNTS"},
		"accounts not a number":      {map[string]string{"WALLET_MAX_ACCOUNTS": "many"}, "WALLET_MAX_ACCOUNTS"},
		"negative workdays":          {map[string]string{"WALLET_OPERATION_TIMEOUT_WORKDAYS": "-1"}, "WALLET_OPERATION_TIMEOUT_WORKDAYS"},
		"workdays not a number":      {map[string]string{"WALLET_OPERATION_TIMEOUT_WORKDAYS": "5.5"}, "WALLET_OPERATION_TIMEOUT_WORKDAYS"},
		"missing password file":      {map[string]string{"WALLET_DATABASE_PASSWORD_FILE": "/nonexistent/password"}, "WALLET_DATABASE_PASSWORD_FILE"},
	}

	for name, tc := range tests {
		values := minimal()
		for k, v := range tc.change {
			values[k] = v
		}

		if _, err := config.Load(env(values)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestDisabledOTPNeedsNoCodeButStaticAlwaysDoes(t *testing.T) {
	values := minimal()
	delete(values, "WALLET_OTP_STATIC_CODE")

	if _, err := config.Load(env(values)); err == nil {
		t.Error("the default mode is static, which must not start without a code")
	}

	values["WALLET_OTP_MODE"] = "disabled"
	if _, err := config.Load(env(values)); err != nil {
		t.Errorf("disabled needs no code: %v", err)
	}
}

func TestTheDatabasePasswordCanComeFromAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	values := minimal()
	values["WALLET_DATABASE_PASSWORD_FILE"] = file

	cfg, err := config.Load(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePassword != "s3cret" {
		t.Errorf("password = %q, want it trimmed", cfg.DatabasePassword)
	}
	if strings.Contains(cfg.DatabaseURL, "s3cret") {
		t.Error("the password must stay out of the URL")
	}
}
