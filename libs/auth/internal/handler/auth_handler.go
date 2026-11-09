// Package handler maps the Auth gRPC API onto the token service. It is the only
// layer that touches transport.
package handler

import (
	"context"
	"errors"
	"time"

	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TokenService is what the handler needs from the service layer.
type TokenService interface {
	Issue(subject string) (token string, ttl time.Duration, err error)
	Verify(token string) (domain.Claims, error)
}

// AuthHandler implements authv1.AuthServer.
type AuthHandler struct {
	authv1.UnimplementedAuthServer
	tokens TokenService
}

// NewAuthHandler returns an AuthHandler backed by tokens.
func NewAuthHandler(tokens TokenService) *AuthHandler {
	return &AuthHandler{tokens: tokens}
}

// Issue signs an access token for the requested subject.
func (h *AuthHandler) Issue(_ context.Context, req *authv1.IssueRequest) (*authv1.IssueResponse, error) {
	token, ttl, err := h.tokens.Issue(req.GetSubject())
	switch {
	case errors.Is(err, domain.ErrEmptySubject):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		// Do not leak signing details to callers.
		return nil, status.Error(codes.Internal, "failed to issue token")
	}

	return &authv1.IssueResponse{AccessToken: token, ExpiresIn: int64(ttl.Seconds())}, nil
}

// Verify checks an access token and returns its subject and expiry.
func (h *AuthHandler) Verify(_ context.Context, req *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
	claims, err := h.tokens.Verify(req.GetAccessToken())
	switch {
	case errors.Is(err, domain.ErrExpiredToken):
		return nil, status.Error(codes.Unauthenticated, err.Error())
	case err != nil:
		return nil, status.Error(codes.Unauthenticated, domain.ErrInvalidToken.Error())
	}

	return &authv1.VerifyResponse{Subject: claims.Subject, ExpiresAt: claims.ExpiresAt}, nil
}
