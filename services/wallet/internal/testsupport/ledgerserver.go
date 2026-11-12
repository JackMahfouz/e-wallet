package testsupport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// LedgerServer serves the real ledger's gRPC API on top of the Ledger fake, so the wallet's
// ledger client can be tested over a real connection, with the real wire format and status codes.
type LedgerServer struct {
	ledgerv1.UnimplementedLedgerServer
	Ledger *Ledger
	// Delay makes every call wait this long first, to test timeouts.
	Delay time.Duration
}

func (s *LedgerServer) wait(ctx context.Context) error {
	if s.Delay == 0 {
		return nil
	}

	select {
	case <-time.After(s.Delay):
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (s *LedgerServer) Debit(ctx context.Context, req *ledgerv1.PostRequest) (*ledgerv1.PostResponse, error) {
	return s.post(ctx, req, s.Ledger.Debit)
}

func (s *LedgerServer) Credit(ctx context.Context, req *ledgerv1.PostRequest) (*ledgerv1.PostResponse, error) {
	return s.post(ctx, req, s.Ledger.Credit)
}

func (s *LedgerServer) post(ctx context.Context, req *ledgerv1.PostRequest, do func(context.Context, string, string, int64) (application.LedgerPosting, error)) (*ledgerv1.PostResponse, error) {
	if err := s.wait(ctx); err != nil {
		return nil, err
	}

	cents, err := processmoney.ToMinorUnits(req.GetAmount())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	posting, err := do(ctx, req.GetAccountId(), req.GetTransactionId(), cents)
	if err != nil {
		return nil, grpcError(err)
	}

	return &ledgerv1.PostResponse{
		Balance:  processmoney.FromMinorUnits(s.Ledger.BalanceOf(req.GetAccountId())),
		Replayed: posting.Replayed,
	}, nil
}

func (s *LedgerServer) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	if err := s.wait(ctx); err != nil {
		return nil, err
	}

	cents, err := s.Ledger.Balance(ctx, req.GetAccountId())
	if err != nil {
		return nil, grpcError(err)
	}

	return &ledgerv1.GetBalanceResponse{Balance: processmoney.FromMinorUnits(cents)}, nil
}

// grpcError gives the codes the real ledger uses.
func grpcError(err error) error {
	var insufficient *domain.InsufficientFundsError

	switch {
	case errors.As(err, &insufficient):
		return status.Error(codes.FailedPrecondition, fmt.Sprintf("insufficient funds: balance %.2f, requested %.2f",
			processmoney.FromMinorUnits(insufficient.Available), processmoney.FromMinorUnits(insufficient.Requested)))
	case errors.Is(err, domain.ErrRejected):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, domain.ErrUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

// StartLedgerServer serves the fake ledger on a loopback port and returns a client for it. The
// server stops when the test ends.
func StartLedgerServer(t testing.TB, server *LedgerServer) ledgerv1.LedgerClient {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	ledgerv1.RegisterLedgerServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return ledgerv1.NewLedgerClient(conn)
}
