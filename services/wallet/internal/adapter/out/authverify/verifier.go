// Package authverify is the driven adapter that checks bearer tokens with the auth service over
// gRPC. It implements application.TokenVerifier.
package authverify

import (
	"context"
	"fmt"
	"time"

	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Verifier asks the auth service whether a token is genuine and whose it is.
type Verifier struct {
	auth    authv1.AuthClient
	timeout time.Duration
}

var _ application.TokenVerifier = (*Verifier)(nil)

// New returns a Verifier that gives each call at most timeout.
func New(auth authv1.AuthClient, timeout time.Duration) *Verifier {
	return &Verifier{auth: auth, timeout: timeout}
}

// Verify implements application.TokenVerifier.
func (v *Verifier) Verify(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	resp, err := v.auth.Verify(ctx, &authv1.VerifyRequest{AccessToken: token})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			return "", domain.ErrUnauthenticated
		}

		// Anything else is the auth service being unreachable or failing: the caller is not told
		// their token is bad, because it may be fine.
		return "", fmt.Errorf("%w: auth service: %v", domain.ErrUnavailable, err)
	}

	return resp.GetSubject(), nil
}
