// Package domain holds the token vocabulary. It has no infrastructure imports.
package domain

import "errors"

var (
	// ErrInvalidToken is returned for any token that is malformed, forged or not ours.
	ErrInvalidToken = errors.New("invalid token")
	// ErrExpiredToken is returned for a genuine token whose lifetime has ended.
	ErrExpiredToken = errors.New("token expired")
	// ErrEmptySubject is returned when asked to issue a token for nobody.
	ErrEmptySubject = errors.New("subject is required")
)

// Claims are the JWT claims carried by an access token.
type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}
