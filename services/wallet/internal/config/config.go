// Package config loads the wallet server settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// OTP modes.
const (
	OTPStatic   = "static"
	OTPDisabled = "disabled"
)

// Funding modes.
const (
	FundingDisabled  = "disabled"
	FundingSimulated = "simulated"
)

const (
	defaultHTTPAddr   = "127.0.0.1:8081"
	defaultGRPCAddr   = "127.0.0.1:9092"
	defaultLedgerAddr = "127.0.0.1:9091"
	defaultAuthAddr   = "127.0.0.1:9090"
)

// Config is the wallet server configuration.
type Config struct {
	// HTTPAddr is the REST listen address, which the gateway routes to. GRPCAddr is the internal
	// gRPC address for other services, which must never be routed through the gateway. Both
	// default to loopback.
	HTTPAddr string
	GRPCAddr string

	// DatabaseURL is the PostgreSQL connection string. Put the password in the file named by
	// WALLET_DATABASE_PASSWORD_FILE instead, so it need not sit in the environment.
	DatabaseURL      string
	DatabasePassword string
	Migrate          bool

	LedgerAddr    string
	LedgerTimeout time.Duration
	AuthAddr      string
	AuthTimeout   time.Duration

	// OTPMode is OTPStatic (one configured code, until libs/auth issues real ones) or OTPDisabled
	// (every code is accepted: for local development only).
	OTPMode       string
	OTPStaticCode string

	// FundingMode is FundingDisabled (deposits and withdrawals are refused) or FundingSimulated
	// (they succeed immediately, with no real money behind them).
	FundingMode string

	MaxAccounts     int
	MaxHoldTTL      time.Duration
	ConfirmTTL      time.Duration
	TimeoutWorkdays int

	// RecoveryInterval is how often the recovery job runs; zero turns it off. RecoveryMinAge is
	// how long a new operation is left alone for the request that made it.
	RecoveryInterval time.Duration
	RecoveryMinAge   time.Duration
}

// Load reads the configuration through getenv (pass os.Getenv outside of tests).
//
//	WALLET_HTTP_ADDR                   REST listen address              (default 127.0.0.1:8081)
//	WALLET_GRPC_ADDR                   internal gRPC listen address     (default 127.0.0.1:9092)
//	WALLET_DATABASE_URL                PostgreSQL connection string     (required)
//	WALLET_DATABASE_PASSWORD_FILE      file holding the password        (optional)
//	WALLET_MIGRATE                     apply migrations at startup      (default true)
//	WALLET_LEDGER_ADDR                 ledger gRPC address              (default 127.0.0.1:9091)
//	WALLET_LEDGER_TIMEOUT              per-call ledger timeout          (default 3s)
//	WALLET_AUTH_ADDR                   auth service gRPC address        (default 127.0.0.1:9090)
//	WALLET_AUTH_TIMEOUT                per-call auth timeout            (default 2s)
//	WALLET_OTP_MODE                    static or disabled               (default static)
//	WALLET_OTP_STATIC_CODE             the code, for static mode        (required for static)
//	WALLET_FUNDING_MODE                disabled or simulated            (default disabled)
//	WALLET_MAX_ACCOUNTS                open accounts per wallet         (default 10)
//	WALLET_MAX_HOLD_TTL                longest a hold may last          (default 168h)
//	WALLET_CONFIRM_TTL                 over-limit confirmation window   (default 10m)
//	WALLET_OPERATION_TIMEOUT_WORKDAYS  working days before failing      (default 5; 0 is immediate)
//	WALLET_RECOVERY_INTERVAL           how often recovery runs          (default 15s; 0 disables)
//	WALLET_RECOVERY_MIN_AGE            leave new operations alone for   (default 30s)
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:    valueOr(getenv("WALLET_HTTP_ADDR"), defaultHTTPAddr),
		GRPCAddr:    valueOr(getenv("WALLET_GRPC_ADDR"), defaultGRPCAddr),
		DatabaseURL: strings.TrimSpace(getenv("WALLET_DATABASE_URL")),
		Migrate:     true,
		LedgerAddr:  valueOr(getenv("WALLET_LEDGER_ADDR"), defaultLedgerAddr),
		AuthAddr:    valueOr(getenv("WALLET_AUTH_ADDR"), defaultAuthAddr),
		OTPMode:     strings.ToLower(valueOr(getenv("WALLET_OTP_MODE"), OTPStatic)),
		FundingMode: strings.ToLower(valueOr(getenv("WALLET_FUNDING_MODE"), FundingDisabled)),

		OTPStaticCode: getenv("WALLET_OTP_STATIC_CODE"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("WALLET_DATABASE_URL is required")
	}

	if raw := getenv("WALLET_MIGRATE"); raw != "" {
		migrate, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("WALLET_MIGRATE must be true or false, got %q", raw)
		}
		cfg.Migrate = migrate
	}

	switch cfg.OTPMode {
	case OTPStatic:
		if cfg.OTPStaticCode == "" {
			return Config{}, errors.New("WALLET_OTP_STATIC_CODE is required when WALLET_OTP_MODE is static (or set WALLET_OTP_MODE=disabled for local development)")
		}
	case OTPDisabled:
	default:
		return Config{}, fmt.Errorf("WALLET_OTP_MODE must be static or disabled, got %q", cfg.OTPMode)
	}

	switch cfg.FundingMode {
	case FundingDisabled, FundingSimulated:
	default:
		return Config{}, fmt.Errorf("WALLET_FUNDING_MODE must be disabled or simulated, got %q", cfg.FundingMode)
	}

	var err error
	if cfg.LedgerTimeout, err = parseDuration(getenv, "WALLET_LEDGER_TIMEOUT", 3*time.Second, false); err != nil {
		return Config{}, err
	}
	if cfg.AuthTimeout, err = parseDuration(getenv, "WALLET_AUTH_TIMEOUT", 2*time.Second, false); err != nil {
		return Config{}, err
	}
	if cfg.MaxHoldTTL, err = parseDuration(getenv, "WALLET_MAX_HOLD_TTL", 168*time.Hour, false); err != nil {
		return Config{}, err
	}
	if cfg.ConfirmTTL, err = parseDuration(getenv, "WALLET_CONFIRM_TTL", 10*time.Minute, false); err != nil {
		return Config{}, err
	}
	if cfg.RecoveryInterval, err = parseDuration(getenv, "WALLET_RECOVERY_INTERVAL", 15*time.Second, true); err != nil {
		return Config{}, err
	}
	if cfg.RecoveryMinAge, err = parseDuration(getenv, "WALLET_RECOVERY_MIN_AGE", 30*time.Second, true); err != nil {
		return Config{}, err
	}
	if cfg.MaxAccounts, err = parseInt(getenv, "WALLET_MAX_ACCOUNTS", 10, 1); err != nil {
		return Config{}, err
	}
	if cfg.TimeoutWorkdays, err = parseInt(getenv, "WALLET_OPERATION_TIMEOUT_WORKDAYS", 5, 0); err != nil {
		return Config{}, err
	}

	if file := getenv("WALLET_DATABASE_PASSWORD_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return Config{}, fmt.Errorf("read WALLET_DATABASE_PASSWORD_FILE: %w", err)
		}
		cfg.DatabasePassword = strings.TrimSpace(string(data))
	}

	return cfg, nil
}

// parseDuration reads a Go duration; it must be positive unless allowZero.
func parseDuration(getenv func(string) string, name string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 || (value == 0 && !allowZero) {
		kind := "positive"
		if allowZero {
			kind = "non-negative"
		}

		return 0, fmt.Errorf("%s must be a %s duration such as 30s or 1h, got %q", name, kind, raw)
	}

	return value, nil
}

func parseInt(getenv func(string) string, name string, fallback, minimum int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum {
		return 0, fmt.Errorf("%s must be a whole number of at least %d, got %q", name, minimum, raw)
	}

	return value, nil
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return strings.TrimSpace(value)
}
