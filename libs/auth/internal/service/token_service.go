// Package service implements token issuing and verification (RS256 JWTs, stdlib crypto only).
package service

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/JackMahfouz/e-wallet/libs/auth/internal/domain"
)

const algorithm = "RS256"

// encoding is canonical base64url without padding. Strict rejects non-canonical
// trailing bits, so a signature has exactly one valid string form.
var encoding = base64.RawURLEncoding.Strict()

var encodedHeader = encoding.EncodeToString([]byte(`{"alg":"` + algorithm + `","typ":"JWT"}`))

// TokenService signs and verifies access tokens with one RSA key.
type TokenService struct {
	key    *rsa.PrivateKey
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

// NewTokenService returns a TokenService. Pass time.Now as now outside of tests.
func NewTokenService(key *rsa.PrivateKey, issuer string, ttl time.Duration, now func() time.Time) *TokenService {
	return &TokenService{key: key, issuer: issuer, ttl: ttl, now: now}
}

// Issue signs a token for subject and returns it with its lifetime.
func (s *TokenService) Issue(subject string) (string, time.Duration, error) {
	if subject == "" {
		return "", 0, domain.ErrEmptySubject
	}

	issuedAt := s.now()
	payload, err := json.Marshal(domain.Claims{
		Issuer:    s.issuer,
		Subject:   subject,
		IssuedAt:  issuedAt.Unix(),
		ExpiresAt: issuedAt.Add(s.ttl).Unix(),
	})
	if err != nil {
		return "", 0, fmt.Errorf("encode claims: %w", err)
	}

	signingInput := encodedHeader + "." + encoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))

	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", 0, fmt.Errorf("sign token: %w", err)
	}

	return signingInput + "." + encoding.EncodeToString(signature), s.ttl, nil
}

// Verify returns the claims of a token we issued that has not expired.
// It returns domain.ErrExpiredToken for an expired token and domain.ErrInvalidToken otherwise.
func (s *TokenService) Verify(token string) (domain.Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return domain.Claims{}, domain.ErrInvalidToken
	}

	// The algorithm is fixed by us, never taken from the token. Rejecting any other
	// header value shuts out "none" and key-confusion (HS256 with the public key) attacks.
	var header struct {
		Alg string `json:"alg"`
	}
	if err := decodeJSON(parts[0], &header); err != nil || header.Alg != algorithm {
		return domain.Claims{}, domain.ErrInvalidToken
	}

	signature, err := encoding.DecodeString(parts[2])
	if err != nil {
		return domain.Claims{}, domain.ErrInvalidToken
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		return domain.Claims{}, domain.ErrInvalidToken
	}

	var claims domain.Claims
	if err := decodeJSON(parts[1], &claims); err != nil {
		return domain.Claims{}, domain.ErrInvalidToken
	}

	if claims.Issuer != s.issuer || claims.Subject == "" || claims.ExpiresAt <= 0 {
		return domain.Claims{}, domain.ErrInvalidToken
	}

	if s.now().Unix() >= claims.ExpiresAt {
		return domain.Claims{}, domain.ErrExpiredToken
	}

	return claims, nil
}

func decodeJSON(segment string, into any) error {
	raw, err := encoding.DecodeString(segment)
	if err != nil {
		return err
	}

	return json.Unmarshal(raw, into)
}
