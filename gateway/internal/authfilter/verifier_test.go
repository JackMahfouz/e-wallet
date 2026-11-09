package authfilter_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/gateway/internal/authfilter"
	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// fakeAuth is a stand-in for libs/auth with a scriptable Verify.
type fakeAuth struct {
	authv1.UnimplementedAuthServer
	verify func(context.Context, *authv1.VerifyRequest) (*authv1.VerifyResponse, error)
}

func (f *fakeAuth) Verify(ctx context.Context, req *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
	return f.verify(ctx, req)
}

// newVerifier serves fake over a real loopback gRPC connection and returns a verifier for it.
func newVerifier(t *testing.T, timeout time.Duration, fake *fakeAuth) *authfilter.GRPC {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	authv1.RegisterAuthServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return authfilter.NewGRPC(authv1.NewAuthClient(conn), timeout)
}

func TestVerifyReturnsTheSubjectOfAnAcceptedToken(t *testing.T) {
	var received string
	verifier := newVerifier(t, time.Second, &fakeAuth{verify: func(_ context.Context, req *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
		received = req.GetAccessToken()

		return &authv1.VerifyResponse{Subject: "42", ExpiresAt: 1794232780}, nil
	}})

	subject, err := verifier.Verify(context.Background(), "the.jwt.token")

	if err != nil || subject != "42" {
		t.Fatalf("Verify = %q, %v; want 42, nil", subject, err)
	}
	if received != "the.jwt.token" {
		t.Errorf("auth service received %q, want the token to be sent unchanged", received)
	}
}

func TestVerifyReportsARejectedTokenAsUnauthenticated(t *testing.T) {
	verifier := newVerifier(t, time.Second, &fakeAuth{verify: func(context.Context, *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
		return nil, status.Error(codes.Unauthenticated, "token expired")
	}})

	subject, err := verifier.Verify(context.Background(), "expired.jwt.token")

	if !errors.Is(err, authfilter.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
	if subject != "" {
		t.Errorf("subject = %q, want empty", subject)
	}
}

func TestVerifyDoesNotMistakeAFailureForARejection(t *testing.T) {
	tests := map[string]codes.Code{
		"unavailable":       codes.Unavailable,
		"internal":          codes.Internal,
		"deadline exceeded": codes.DeadlineExceeded,
		"permission denied": codes.PermissionDenied,
	}

	for name, code := range tests {
		t.Run(name, func(t *testing.T) {
			verifier := newVerifier(t, time.Second, &fakeAuth{verify: func(context.Context, *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
				return nil, status.Error(code, "boom")
			}})

			_, err := verifier.Verify(context.Background(), "t")

			if err == nil || errors.Is(err, authfilter.ErrUnauthenticated) {
				t.Errorf("err = %v, want a failure that is not ErrUnauthenticated", err)
			}
		})
	}
}

func TestVerifyGivesUpWhenTheAuthServiceIsSlow(t *testing.T) {
	verifier := newVerifier(t, 50*time.Millisecond, &fakeAuth{verify: func(ctx context.Context, _ *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
		select {
		case <-time.After(2 * time.Second):
			return &authv1.VerifyResponse{Subject: "42"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})

	start := time.Now()
	_, err := verifier.Verify(context.Background(), "t")
	elapsed := time.Since(start)

	if err == nil || errors.Is(err, authfilter.ErrUnauthenticated) {
		t.Errorf("err = %v, want a timeout failure that is not ErrUnauthenticated", err)
	}
	if elapsed > time.Second {
		t.Errorf("took %v, want it to give up near the 50ms timeout", elapsed)
	}
}

func TestVerifyRejectsAnAcceptedTokenWithoutASubject(t *testing.T) {
	verifier := newVerifier(t, time.Second, &fakeAuth{verify: func(context.Context, *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
		return &authv1.VerifyResponse{}, nil
	}})

	subject, err := verifier.Verify(context.Background(), "t")

	if err == nil || errors.Is(err, authfilter.ErrUnauthenticated) {
		t.Errorf("err = %v, want a failure that is not ErrUnauthenticated", err)
	}
	if subject != "" {
		t.Errorf("subject = %q, want empty", subject)
	}
}

func TestVerifyFailsWhenTheAuthServiceIsNotRunning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	verifier := authfilter.NewGRPC(authv1.NewAuthClient(conn), 500*time.Millisecond)

	_, err = verifier.Verify(context.Background(), "t")

	if err == nil || errors.Is(err, authfilter.ErrUnauthenticated) {
		t.Errorf("err = %v, want a failure that is not ErrUnauthenticated", err)
	}
}
