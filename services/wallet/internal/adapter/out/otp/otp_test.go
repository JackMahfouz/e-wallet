package otp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/otp"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

func TestStaticAcceptsOnlyTheConfiguredCode(t *testing.T) {
	verifier, err := otp.NewStatic("246810")
	if err != nil {
		t.Fatal(err)
	}

	if err := verifier.Verify(context.Background(), "user", "246810"); err != nil {
		t.Errorf("the right code: %v", err)
	}
	for name, code := range map[string]string{
		"empty": "", "wrong": "123456", "prefix": "24681", "suffix": "2468100", "spaces": " 246810", "case": "246810 ",
	} {
		if err := verifier.Verify(context.Background(), "user", code); !errors.Is(err, domain.ErrOTPInvalid) {
			t.Errorf("%s: err = %v, want ErrOTPInvalid", name, err)
		}
	}
}

func TestStaticRefusesToBeBuiltWithoutACode(t *testing.T) {
	if _, err := otp.NewStatic(""); err == nil {
		t.Error("a static verifier with no code would accept the empty code from everyone")
	}
}

func TestDisabledAcceptsAnything(t *testing.T) {
	for _, code := range []string{"", "x", "123456"} {
		if err := (otp.Disabled{}).Verify(context.Background(), "user", code); err != nil {
			t.Errorf("code %q: %v", code, err)
		}
	}
}
