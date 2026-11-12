// Package grpcapi is the driving adapter that exposes the wallet to other services over gRPC. It
// is internal only: never route it through the API gateway. It translates messages into calls on
// application.Internal and the answers back; it holds no rules of its own.
package grpcapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	walletv1 "github.com/JackMahfouz/e-wallet/services/wallet/gen/wallet/v1"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements walletv1.WalletServer.
type Server struct {
	walletv1.UnimplementedWalletServer
	core application.Internal
	log  *slog.Logger
}

// New returns the gRPC service over the core's internal API.
func New(core application.Internal, log *slog.Logger) *Server {
	return &Server{core: core, log: log}
}

// Credit implements walletv1.WalletServer.
func (s *Server) Credit(ctx context.Context, req *walletv1.MoveRequest) (*walletv1.Operation, error) {
	return s.move(ctx, req, s.core.Credit)
}

// Debit implements walletv1.WalletServer.
func (s *Server) Debit(ctx context.Context, req *walletv1.MoveRequest) (*walletv1.Operation, error) {
	return s.move(ctx, req, s.core.Debit)
}

// Transfer implements walletv1.WalletServer.
func (s *Server) Transfer(ctx context.Context, req *walletv1.MoveRequest) (*walletv1.Operation, error) {
	return s.move(ctx, req, s.core.TransferBetween)
}

func (s *Server) move(ctx context.Context, req *walletv1.MoveRequest, do func(context.Context, application.ServiceMoneyRequest) (domain.Operation, error)) (*walletv1.Operation, error) {
	cents, err := processmoney.ToMinorUnits(req.GetAmount())
	if err != nil {
		return nil, s.status(ctx, err)
	}

	op, err := do(ctx, application.ServiceMoneyRequest{
		Service: req.GetService(), AccountID: req.GetAccountId(), CounterpartyID: req.GetCounterpartyAccountId(),
		Amount: cents, HoldID: req.GetHoldId(), IdempotencyKey: req.GetIdempotencyKey(),
	})

	// A spend over a limit is not an error to the caller: the operation exists, awaiting the owner's
	// confirmation, and its status says so.
	var confirmation *domain.ConfirmationRequiredError
	if err != nil && !errors.As(err, &confirmation) {
		return nil, s.status(ctx, err)
	}

	return operationOut(op), nil
}

// GetOperation implements walletv1.WalletServer.
func (s *Server) GetOperation(ctx context.Context, req *walletv1.GetOperationRequest) (*walletv1.Operation, error) {
	op, err := s.core.OperationFor(ctx, req.GetOperationId())
	if err != nil {
		return nil, s.status(ctx, err)
	}

	return operationOut(op), nil
}

// CreateHold implements walletv1.WalletServer.
func (s *Server) CreateHold(ctx context.Context, req *walletv1.CreateHoldRequest) (*walletv1.Hold, error) {
	cents, err := processmoney.ToMinorUnits(req.GetAmount())
	if err != nil {
		return nil, s.status(ctx, err)
	}

	hold, err := s.core.CreateHold(ctx, application.CreateHoldRequest{
		Service: req.GetService(), AccountID: req.GetAccountId(), Amount: cents, Reason: req.GetReason(),
		TTL: time.Duration(req.GetTtlSeconds()) * time.Second, IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, s.status(ctx, err)
	}

	return holdOut(hold), nil
}

// ReleaseHold implements walletv1.WalletServer.
func (s *Server) ReleaseHold(ctx context.Context, req *walletv1.ReleaseHoldRequest) (*walletv1.Hold, error) {
	hold, err := s.core.ReleaseHold(ctx, req.GetService(), req.GetHoldId())
	if err != nil {
		return nil, s.status(ctx, err)
	}

	return holdOut(hold), nil
}

// GetAccount implements walletv1.WalletServer.
func (s *Server) GetAccount(ctx context.Context, req *walletv1.GetAccountRequest) (*walletv1.Account, error) {
	account, err := s.core.AccountFor(ctx, req.GetAccountId())
	if err != nil {
		return nil, s.status(ctx, err)
	}

	out := &walletv1.Account{
		Id: account.ID, WalletId: account.WalletID, Name: account.Name,
		Status:  walletv1.AccountStatus(account.Status),
		Balance: processmoney.FromMinorUnits(account.Balance), Held: processmoney.FromMinorUnits(account.Held),
		Available: processmoney.FromMinorUnits(account.Available), SpentToday: processmoney.FromMinorUnits(account.SpentToday),
	}
	if account.DailyLimit != nil {
		out.DailyLimit, out.HasDailyLimit = processmoney.FromMinorUnits(*account.DailyLimit), true
	}

	return out, nil
}

// status turns a core error into a gRPC status. Messages for the caller's own mistakes come from
// the core and are safe to show; anything unexpected is logged and reported generically.
func (s *Server) status(ctx context.Context, err error) error {
	var insufficient *domain.InsufficientFundsError

	switch {
	case errors.Is(err, processmoney.ErrInvalidAmount), errors.Is(err, domain.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.As(err, &insufficient):
		return status.Errorf(codes.FailedPrecondition, "insufficient funds: %.2f available, %.2f requested",
			processmoney.FromMinorUnits(insufficient.Available), processmoney.FromMinorUnits(insufficient.Requested))
	case errors.Is(err, domain.ErrAccountNotActive), errors.Is(err, domain.ErrConflict):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, domain.ErrUnavailable):
		return status.Error(codes.Unavailable, "a service this depends on is not available")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request cancelled")
	default:
		s.log.ErrorContext(ctx, "wallet request failed", "error", err)

		return status.Error(codes.Internal, "internal error")
	}
}

func operationOut(op domain.Operation) *walletv1.Operation {
	out := &walletv1.Operation{
		Id: op.ID, Kind: walletv1.OperationKind(op.Kind), Status: walletv1.OperationStatus(op.Status),
		FailureReason: walletv1.FailureReason(op.Failure), AccountId: op.AccountID,
		CounterpartyAccountId: op.CounterpartyID, Amount: processmoney.FromMinorUnits(op.Amount),
		Spending: op.Spending, Initiator: op.Initiator, CreatedAtMicros: op.CreatedAt.UnixMicro(),
		UpdatedAtMicros: op.UpdatedAt.UnixMicro(), DeadlineAtMicros: op.DeadlineAt.UnixMicro(),
	}
	if op.CompletedAt != nil {
		out.CompletedAtMicros = op.CompletedAt.UnixMicro()
	}
	if op.Status == domain.StatusAwaitingConfirmation && op.ConfirmExpiresAt != nil {
		out.Confirmation = &walletv1.Confirmation{
			LimitScope: walletv1.LimitScope(op.LimitScope), ExpiresAtMicros: op.ConfirmExpiresAt.UnixMicro(),
		}
	}

	return out
}

func holdOut(h domain.Hold) *walletv1.Hold {
	out := &walletv1.Hold{
		Id: h.ID, AccountId: h.AccountID, Amount: processmoney.FromMinorUnits(h.Amount), Reason: h.Reason,
		ExpiresAtMicros: h.ExpiresAt.UnixMicro(), CapturedByOperationId: h.CapturedBy,
	}
	if h.ReleasedAt != nil {
		out.ReleasedAtMicros = h.ReleasedAt.UnixMicro()
	}

	return out
}

// LoggingInterceptor logs one line per call: the method, the status code and how long it took. It
// never logs the request, which carries account ids and amounts.
func LoggingInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)

		log.InfoContext(ctx, "grpc call", "method", info.FullMethod, "code", status.Code(err).String(),
			"duration_ms", time.Since(start).Milliseconds())

		return resp, err
	}
}
