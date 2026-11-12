package authverify_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/authverify"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type fakeAuth struct {
	authv1.UnimplementedAuthServer
	verify func(context.Context, string) (string, error)
}

func (f fakeAuth) Verify(ctx context.Context, req *authv1.VerifyRequest) (*authv1.VerifyResponse, error) {
	subject, err := f.verify(ctx, req.GetAccessToken())
	if err != nil {
		return nil, err
	}

	return &authv1.VerifyResponse{Subject: subject, ExpiresAt: time.Now().Add(time.Minute).Unix()}, nil
}

func start(t *testing.T, verify func(context.Context, string) (string, error), timeout time.Duration) *authverify.Verifier {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	authv1.RegisterAuthServer(server, fakeAuth{verify: verify})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return authverify.New(authv1.NewAuthClient(conn), timeout)
}

func TestAGoodTokenYieldsItsSubject(t *testing.T) {
	v := start(t, func(_ context.Context, token string) (string, error) {
		if token != "good" {
			t.Errorf("token sent = %q", token)
		}

		return "3f2504e0-4f89-41d3-9a0c-0305e82c3301", nil
	}, time.Second)

	subject, err := v.Verify(context.Background(), "good")
	if err != nil || subject != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Errorf("Verify = %q, %v", subject, err)
	}
}

func TestARejectedTokenIsUnauthenticated(t *testing.T) {
	v := start(t, func(context.Context, string) (string, error) {
		return "", status.Error(codes.Unauthenticated, "invalid token")
	}, time.Second)

	if _, err := v.Verify(context.Background(), "bad"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestAnAuthServiceThatFailsOrIsSlowIsUnavailableNotUnauthenticated(t *testing.T) {
	failing := start(t, func(context.Context, string) (string, error) {
		return "", status.Error(codes.Internal, "boom")
	}, time.Second)
	if _, err := failing.Verify(context.Background(), "x"); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("failing service: err = %v, want ErrUnavailable", err)
	}

	slow := start(t, func(ctx context.Context, _ string) (string, error) {
		select {
		case <-time.After(time.Second):
			return "x", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}, 100*time.Millisecond)
	if _, err := slow.Verify(context.Background(), "x"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("slow service: err = %v, want ErrUnavailable", err)
	}

	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	dead := authverify.New(authv1.NewAuthClient(conn), 300*time.Millisecond)
	if _, err := dead.Verify(context.Background(), "x"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("dead service: err = %v, want ErrUnavailable", err)
	}
}
