// Package authfilter checks access tokens by asking the internal libs/auth service.
package authfilter

import (
	"context"
	"errors"
	"fmt"
	"time"

	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrUnauthenticated means the auth service looked at the token and rejected it. Every other
// error means the check could not be made, which callers must treat as a failure, never as
// "allowed".
var ErrUnauthenticated = errors.New("unauthenticated")

// Verifier returns the subject of a valid token.
type Verifier interface {
	Verify(ctx context.Context, token string) (subject string, err error)
}

// GRPC verifies tokens over gRPC against libs/auth.
type GRPC struct {
	client  authv1.AuthClient
	timeout time.Duration
}

// NewGRPC returns a verifier. Each call is bounded by timeout.
func NewGRPC(client authv1.AuthClient, timeout time.Duration) *GRPC {
	return &GRPC{client: client, timeout: timeout}
}

// Verify implements Verifier.
func (g *GRPC) Verify(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	response, err := g.client.Verify(ctx, &authv1.VerifyRequest{AccessToken: token})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			return "", ErrUnauthenticated
		}

		return "", fmt.Errorf("verify token: %w", err)
	}

	// An accepted token always has a subject; an empty one means the auth service misbehaved.
	if response.GetSubject() == "" {
		return "", errors.New("verify token: auth service accepted the token without a subject")
	}

	return response.GetSubject(), nil
}
