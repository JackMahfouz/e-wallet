// Package config loads the auth server settings from the environment.
package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	defaultAddr   = "127.0.0.1:9090"
	defaultIssuer = "e-wallet"
	defaultTTL    = 15 * time.Minute
	minKeyBits    = 2048
)

// Config is the auth server configuration.
type Config struct {
	// GRPCAddr is the internal listen address. It defaults to loopback; in a cluster set
	// it to 0.0.0.0:9090 behind a ClusterIP Service that the gateway never routes to.
	GRPCAddr   string
	PrivateKey *rsa.PrivateKey
	Issuer     string
	AccessTTL  time.Duration
}

// Load reads the configuration through getenv (pass os.Getenv outside of tests).
//
//	AUTH_GRPC_ADDR         listen address            (default 127.0.0.1:9090)
//	AUTH_PRIVATE_KEY_FILE  PEM RSA private key path  (required; a file, not an env value)
//	AUTH_ISSUER            "iss" claim               (default e-wallet)
//	AUTH_ACCESS_TTL        access token lifetime     (default 15m, Go duration)
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		GRPCAddr:  valueOr(getenv("AUTH_GRPC_ADDR"), defaultAddr),
		Issuer:    valueOr(getenv("AUTH_ISSUER"), defaultIssuer),
		AccessTTL: defaultTTL,
	}

	if raw := getenv("AUTH_ACCESS_TTL"); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl <= 0 {
			return Config{}, fmt.Errorf("AUTH_ACCESS_TTL must be a positive duration such as 15m, got %q", raw)
		}
		cfg.AccessTTL = ttl
	}

	path := getenv("AUTH_PRIVATE_KEY_FILE")
	if path == "" {
		return Config{}, errors.New("AUTH_PRIVATE_KEY_FILE is required")
	}

	key, err := loadPrivateKey(path)
	if err != nil {
		return Config{}, err
	}
	cfg.PrivateKey = key

	return cfg, nil
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("private key %s is not PEM encoded", path)
	}

	key, err := parseRSAKey(block)
	if err != nil {
		return nil, fmt.Errorf("private key %s: %w", path, err)
	}

	if bits := key.N.BitLen(); bits < minKeyBits {
		return nil, fmt.Errorf("private key %s is %d bits, need at least %d", path, bits, minKeyBits)
	}

	return key, nil
}

// parseRSAKey accepts PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY"), which is
// what `openssl genrsa` writes depending on the OpenSSL version.
func parseRSAKey(block *pem.Block) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not a valid RSA private key")
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}

	return key, nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
