package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/libs/auth/internal/config"
)

var testKey = mustRSAKey(2048)

func mustRSAKey(bits int) *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		panic(err)
	}

	return key
}

func writePEM(t *testing.T, blockType string, der []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return path
}

func pkcs8(t *testing.T, key any) []byte {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}

	return der
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writePEM(t, "PRIVATE KEY", pkcs8(t, testKey))

	cfg, err := config.Load(env(map[string]string{"AUTH_PRIVATE_KEY_FILE": path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.GRPCAddr != "127.0.0.1:9090" {
		t.Errorf("GRPCAddr = %q, want loopback 127.0.0.1:9090", cfg.GRPCAddr)
	}
	if cfg.Issuer != "e-wallet" {
		t.Errorf("Issuer = %q, want e-wallet", cfg.Issuer)
	}
	if cfg.AccessTTL != 15*time.Minute {
		t.Errorf("AccessTTL = %v, want 15m", cfg.AccessTTL)
	}
	if !cfg.PrivateKey.Equal(testKey) {
		t.Error("PrivateKey does not match the key on disk")
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	path := writePEM(t, "PRIVATE KEY", pkcs8(t, testKey))

	cfg, err := config.Load(env(map[string]string{
		"AUTH_PRIVATE_KEY_FILE": path,
		"AUTH_GRPC_ADDR":        "0.0.0.0:7000",
		"AUTH_ISSUER":           "wallet-auth",
		"AUTH_ACCESS_TTL":       "90s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.GRPCAddr != "0.0.0.0:7000" || cfg.Issuer != "wallet-auth" || cfg.AccessTTL != 90*time.Second {
		t.Errorf("cfg = %+v, want the overridden values", cfg)
	}
}

func TestLoadAcceptsPKCS1AndPKCS8Keys(t *testing.T) {
	tests := map[string]string{
		"pkcs1": writePEM(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(testKey)),
		"pkcs8": writePEM(t, "PRIVATE KEY", pkcs8(t, testKey)),
	}

	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := config.Load(env(map[string]string{"AUTH_PRIVATE_KEY_FILE": path}))

			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !cfg.PrivateKey.Equal(testKey) {
				t.Error("PrivateKey does not match the key on disk")
			}
		})
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}

	validKey := writePEM(t, "PRIVATE KEY", pkcs8(t, testKey))
	notPEM := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(notPEM, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tests := map[string]struct {
		env     map[string]string
		message string
	}{
		"key file setting missing": {
			env:     map[string]string{},
			message: "AUTH_PRIVATE_KEY_FILE is required",
		},
		"key file does not exist": {
			env:     map[string]string{"AUTH_PRIVATE_KEY_FILE": filepath.Join(t.TempDir(), "missing.pem")},
			message: "read private key",
		},
		"key file is not PEM": {
			env:     map[string]string{"AUTH_PRIVATE_KEY_FILE": notPEM},
			message: "not PEM encoded",
		},
		"key is not RSA": {
			env:     map[string]string{"AUTH_PRIVATE_KEY_FILE": writePEM(t, "PRIVATE KEY", pkcs8(t, ecKey))},
			message: "not an RSA key",
		},
		"key is too small": {
			env:     map[string]string{"AUTH_PRIVATE_KEY_FILE": writePEM(t, "PRIVATE KEY", pkcs8(t, mustRSAKey(1024)))},
			message: "need at least 2048",
		},
		"ttl is not a duration": {
			env:     map[string]string{"AUTH_PRIVATE_KEY_FILE": validKey, "AUTH_ACCESS_TTL": "soon"},
			message: "AUTH_ACCESS_TTL",
		},
		"ttl is zero": {
			env:     map[string]string{"AUTH_PRIVATE_KEY_FILE": validKey, "AUTH_ACCESS_TTL": "0s"},
			message: "AUTH_ACCESS_TTL",
		},
		"ttl is negative": {
			env:     map[string]string{"AUTH_PRIVATE_KEY_FILE": validKey, "AUTH_ACCESS_TTL": "-5m"},
			message: "AUTH_ACCESS_TTL",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(env(tc.env))

			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %q, want it to contain %q", err, tc.message)
			}
		})
	}
}
