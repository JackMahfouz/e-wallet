package handler_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/domain"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/handler"
	"github.com/JackMahfouz/e-wallet/libs/auth/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const ttl = 15 * time.Minute

var (
	testKey = mustKey()
	epoch   = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

func mustKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return key
}

// clock is safe to move while the server goroutines read it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newClient starts the real service on a loopback port and returns a gRPC client for it.
func newClient(t *testing.T, c *clock) authv1.AuthClient {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := grpc.NewServer()
	tokens := service.NewTokenService(testKey, "e-wallet", ttl, c.Now)
	authv1.RegisterAuthServer(server, handler.NewAuthHandler(tokens))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return authv1.NewAuthClient(conn)
}

func TestIssueThenVerifyOverGRPC(t *testing.T) {
	client := newClient(t, &clock{now: epoch})

	issued, err := client.Issue(context.Background(), &authv1.IssueRequest{Subject: "42"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	verified, err := client.Verify(context.Background(), &authv1.VerifyRequest{AccessToken: issued.GetAccessToken()})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if issued.GetAccessToken() == "" {
		t.Error("access token is empty")
	}
	if issued.GetExpiresIn() != int64(ttl.Seconds()) {
		t.Errorf("expires_in = %d, want %d", issued.GetExpiresIn(), int64(ttl.Seconds()))
	}
	if verified.GetSubject() != "42" {
		t.Errorf("subject = %q, want 42", verified.GetSubject())
	}
	if want := epoch.Add(ttl).Unix(); verified.GetExpiresAt() != want {
		t.Errorf("expires_at = %d, want %d", verified.GetExpiresAt(), want)
	}
}

func TestVerifyReturnsUnauthenticated(t *testing.T) {
	c := &clock{now: epoch}
	client := newClient(t, c)
	expired, err := client.Issue(context.Background(), &authv1.IssueRequest{Subject: "42"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c.Advance(ttl)

	tests := map[string]struct {
		token   string
		message string
	}{
		"garbage token": {token: "garbage", message: "invalid token"},
		"empty token":   {token: "", message: "invalid token"},
		"expired token": {token: expired.GetAccessToken(), message: "token expired"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := client.Verify(context.Background(), &authv1.VerifyRequest{AccessToken: tc.token})

			if got := status.Code(err); got != codes.Unauthenticated {
				t.Errorf("code = %v, want Unauthenticated", got)
			}
			if got := status.Convert(err).Message(); got != tc.message {
				t.Errorf("message = %q, want %q", got, tc.message)
			}
		})
	}
}

func TestIssueRejectsEmptySubjectWithInvalidArgument(t *testing.T) {
	client := newClient(t, &clock{now: epoch})

	_, err := client.Issue(context.Background(), &authv1.IssueRequest{Subject: ""})

	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
}

type failingTokens struct{}

func (failingTokens) Issue(string) (string, time.Duration, error) {
	return "", 0, errors.New("rsa: private key blew up")
}

func (failingTokens) Verify(string) (domain.Claims, error) {
	return domain.Claims{}, errors.New("unexpected")
}

func TestIssueHidesInternalFailureDetails(t *testing.T) {
	h := handler.NewAuthHandler(failingTokens{})

	_, err := h.Issue(context.Background(), &authv1.IssueRequest{Subject: "42"})

	if got := status.Code(err); got != codes.Internal {
		t.Errorf("code = %v, want Internal", got)
	}
	if got := status.Convert(err).Message(); got != "failed to issue token" {
		t.Errorf("message = %q, want a generic message", got)
	}
}

func TestVerifyTreatsUnexpectedFailuresAsUnauthenticated(t *testing.T) {
	h := handler.NewAuthHandler(failingTokens{})

	_, err := h.Verify(context.Background(), &authv1.VerifyRequest{AccessToken: "x"})

	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", got)
	}
	if got := status.Convert(err).Message(); got != "invalid token" {
		t.Errorf("message = %q, want invalid token", got)
	}
}
