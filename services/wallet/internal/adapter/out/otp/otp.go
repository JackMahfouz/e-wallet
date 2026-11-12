// Package otp is the driven adapter that checks the one-time code for sensitive actions.
//
// libs/auth does not issue one-time codes yet. Until it does, Static accepts a single configured
// code. That is a shared secret, not a second factor: it proves nothing about the person, and the
// docs say so. When libs/auth grows real codes, an adapter over it replaces this one and nothing in
// the core changes.
package otp

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// Static accepts one configured code, from every user.
type Static struct {
	code []byte
}

var _ application.OTPVerifier = (*Static)(nil)

// NewStatic returns a verifier for code, which must not be empty.
func NewStatic(code string) (*Static, error) {
	if code == "" {
		return nil, errors.New("the static one-time code must not be empty")
	}

	return &Static{code: []byte(code)}, nil
}

// Verify implements application.OTPVerifier. The comparison takes the same time whatever part of
// the code is right.
func (s *Static) Verify(_ context.Context, _, code string) error {
	if subtle.ConstantTimeCompare(s.code, []byte(code)) != 1 {
		return domain.ErrOTPInvalid
	}

	return nil
}

// Disabled accepts any code, including none. It exists for local development and nothing else.
type Disabled struct{}

var _ application.OTPVerifier = Disabled{}

// Verify implements application.OTPVerifier.
func (Disabled) Verify(context.Context, string, string) error { return nil }
