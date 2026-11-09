package service_test

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/libs/auth/internal/domain"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/service"
)

const (
	issuer      = "e-wallet"
	ttl         = 15 * time.Minute
	rs256Header = `{"alg":"RS256","typ":"JWT"}`
)

var (
	testKey  = mustKey()
	otherKey = mustKey()
	epoch    = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

func mustKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return key
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newService(key *rsa.PrivateKey, iss string, c *clock) *service.TokenService {
	return service.NewTokenService(key, iss, ttl, c.Now)
}

func segment(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// craft builds a token by hand, signed with key, so tests can forge shapes the service never issues.
func craft(key *rsa.PrivateKey, header, claims string) string {
	signingInput := segment(header) + "." + segment(claims)
	digest := sha256.Sum256([]byte(signingInput))

	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func claimsJSON(iss, sub string, exp int64) string {
	return fmt.Sprintf(`{"iss":%q,"sub":%q,"iat":%d,"exp":%d}`, iss, sub, epoch.Unix(), exp)
}

func TestIssueThenVerifyReturnsClaims(t *testing.T) {
	svc := newService(testKey, issuer, &clock{now: epoch})

	token, lifetime, err := svc.Issue("42")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	claims, err := svc.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	want := domain.Claims{Issuer: issuer, Subject: "42", IssuedAt: epoch.Unix(), ExpiresAt: epoch.Add(ttl).Unix()}
	if claims != want {
		t.Errorf("claims = %+v, want %+v", claims, want)
	}
	if lifetime != ttl {
		t.Errorf("lifetime = %v, want %v", lifetime, ttl)
	}
}

func TestIssueProducesStandardRS256Token(t *testing.T) {
	svc := newService(testKey, issuer, &clock{now: epoch})

	token, _, err := svc.Issue("42")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(token, ".")
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(header) != rs256Header {
		t.Fatalf("header = %q (err %v), want %q", header, err, rs256Header)
	}

	// Any other service holding only the public key must be able to verify it, without our code.
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Errorf("signature does not verify with the public key: %v", err)
	}
}

func TestIssueRejectsEmptySubject(t *testing.T) {
	svc := newService(testKey, issuer, &clock{now: epoch})

	_, _, err := svc.Issue("")

	if !errors.Is(err, domain.ErrEmptySubject) {
		t.Errorf("err = %v, want ErrEmptySubject", err)
	}
}

func TestVerifyAcceptsTokenUntilItsExpiry(t *testing.T) {
	c := &clock{now: epoch}
	svc := newService(testKey, issuer, c)
	token, _, err := svc.Issue("42")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	c.now = epoch.Add(ttl - time.Second)
	if _, err := svc.Verify(token); err != nil {
		t.Errorf("one second before expiry: err = %v, want nil", err)
	}

	c.now = epoch.Add(ttl)
	if _, err := svc.Verify(token); !errors.Is(err, domain.ErrExpiredToken) {
		t.Errorf("at expiry: err = %v, want ErrExpiredToken", err)
	}

	c.now = epoch.Add(24 * time.Hour)
	if _, err := svc.Verify(token); !errors.Is(err, domain.ErrExpiredToken) {
		t.Errorf("a day later: err = %v, want ErrExpiredToken", err)
	}
}

func TestVerifyRejectsInvalidTokens(t *testing.T) {
	c := &clock{now: epoch}
	svc := newService(testKey, issuer, c)
	valid, _, err := svc.Issue("42")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	parts := strings.Split(valid, ".")

	foreign, _, err := newService(otherKey, issuer, c).Issue("42")
	if err != nil {
		t.Fatalf("Issue (other key): %v", err)
	}

	publicKeyDER, err := x509.MarshalPKIXPublicKey(&testKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	publicKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKeyDER})
	hsInput := segment(`{"alg":"HS256","typ":"JWT"}`) + "." + segment(claimsJSON(issuer, "42", epoch.Add(ttl).Unix()))
	mac := hmac.New(sha256.New, publicKeyPEM)
	mac.Write([]byte(hsInput))

	exp := epoch.Add(ttl).Unix()
	tests := map[string]string{
		"empty string":                        "",
		"one part":                            "abc",
		"two parts":                           "abc.def",
		"four parts":                          "a.b.c.d",
		"signature is not base64":             parts[0] + "." + parts[1] + ".!!!",
		"signature truncated":                 valid[:len(valid)-10],
		"payload changed after signing":       parts[0] + "." + segment(claimsJSON(issuer, "43", exp)) + "." + parts[2],
		"signed with a different key":         foreign,
		"alg none":                            segment(`{"alg":"none","typ":"JWT"}`) + "." + segment(claimsJSON(issuer, "42", exp)) + ".",
		"alg HS256 keyed with the public key": hsInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
		"alg header is not RS256 despite a valid signature": craft(testKey, `{"alg":"HS256","typ":"JWT"}`, claimsJSON(issuer, "42", exp)),
		"wrong issuer":        craft(testKey, rs256Header, claimsJSON("someone-else", "42", exp)),
		"empty subject":       craft(testKey, rs256Header, claimsJSON(issuer, "", exp)),
		"missing expiry":      craft(testKey, rs256Header, `{"iss":"e-wallet","sub":"42"}`),
		"claims are not json": craft(testKey, rs256Header, "not json"),
	}

	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			claims, err := svc.Verify(token)

			if !errors.Is(err, domain.ErrInvalidToken) {
				t.Errorf("err = %v, want ErrInvalidToken", err)
			}
			if claims != (domain.Claims{}) {
				t.Errorf("claims = %+v, want zero value", claims)
			}
		})
	}
}

func TestVerifyDoesNotLeakClaimsOfExpiredTokens(t *testing.T) {
	c := &clock{now: epoch}
	svc := newService(testKey, issuer, c)
	token, _, err := svc.Issue("42")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	c.now = epoch.Add(ttl)
	claims, err := svc.Verify(token)

	if !errors.Is(err, domain.ErrExpiredToken) {
		t.Fatalf("err = %v, want ErrExpiredToken", err)
	}
	if encoded, _ := json.Marshal(claims); string(encoded) != `{"iss":"","sub":"","iat":0,"exp":0}` {
		t.Errorf("claims = %s, want zero value", encoded)
	}
}
