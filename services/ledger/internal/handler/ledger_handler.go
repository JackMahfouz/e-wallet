// Package handler exposes the ledger over gRPC. It is the only layer that touches transport, and
// the only place decimal amounts exist: it converts them to whole cents (x 100) before anything
// else runs and back (/ 100) when building responses.
package handler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LedgerService is what the handler needs from the service layer.
type LedgerService interface {
	Debit(ctx context.Context, in service.PostInput) (service.PostResult, error)
	Credit(ctx context.Context, in service.PostInput) (service.PostResult, error)
	Balance(ctx context.Context, accountID string) (domain.AccountBalance, error)
	ListEntries(ctx context.Context, accountID string, afterSeq int64, limit int) ([]domain.Entry, bool, error)
	ListCheckpoints(ctx context.Context, accountID string, period domain.Period, limit int) ([]domain.Checkpoint, error)
}

// Handler implements ledgerv1.LedgerServer.
type Handler struct {
	ledgerv1.UnimplementedLedgerServer
	ledger LedgerService
	log    *slog.Logger
}

// New returns a Handler backed by ledger.
func New(ledger LedgerService, log *slog.Logger) *Handler {
	return &Handler{ledger: ledger, log: log}
}

// Debit appends a debit entry.
func (h *Handler) Debit(ctx context.Context, req *ledgerv1.PostRequest) (*ledgerv1.PostResponse, error) {
	return h.post(ctx, req, h.ledger.Debit)
}

// Credit appends a credit entry.
func (h *Handler) Credit(ctx context.Context, req *ledgerv1.PostRequest) (*ledgerv1.PostResponse, error) {
	return h.post(ctx, req, h.ledger.Credit)
}

func (h *Handler) post(
	ctx context.Context,
	req *ledgerv1.PostRequest,
	do func(context.Context, service.PostInput) (service.PostResult, error),
) (*ledgerv1.PostResponse, error) {
	// The amount is multiplied by 100 and checked here, once, and is cents from then on.
	cents, err := processmoney.ToMinorUnits(req.GetAmount())
	if err != nil {
		return nil, h.status(ctx, err)
	}

	result, err := do(ctx, service.PostInput{
		AccountID:     req.GetAccountId(),
		TransactionID: req.GetTransactionId(),
		Amount:        cents,
	})
	if err != nil {
		return nil, h.status(ctx, err)
	}

	return &ledgerv1.PostResponse{
		Entry:    entryToProto(result.Entry),
		Balance:  processmoney.FromMinorUnits(result.Balance),
		Replayed: result.Replayed,
	}, nil
}

// GetBalance returns the account's current balance.
func (h *Handler) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	balance, err := h.ledger.Balance(ctx, req.GetAccountId())
	if err != nil {
		return nil, h.status(ctx, err)
	}

	return &ledgerv1.GetBalanceResponse{Balance: processmoney.FromMinorUnits(balance.Balance), LastSeq: balance.LastSeq}, nil
}

// ListEntries returns a page of an account's entries, oldest first.
func (h *Handler) ListEntries(ctx context.Context, req *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
	entries, hasMore, err := h.ledger.ListEntries(ctx, req.GetAccountId(), req.GetAfterSeq(), int(req.GetLimit()))
	if err != nil {
		return nil, h.status(ctx, err)
	}

	response := &ledgerv1.ListEntriesResponse{HasMore: hasMore}
	for _, entry := range entries {
		response.Entries = append(response.Entries, entryToProto(entry))
	}

	return response, nil
}

// ListCheckpoints returns an account's checkpoints, newest first.
func (h *Handler) ListCheckpoints(ctx context.Context, req *ledgerv1.ListCheckpointsRequest) (*ledgerv1.ListCheckpointsResponse, error) {
	period, err := periodFromProto(req.GetPeriod())
	if err != nil {
		return nil, h.status(ctx, err)
	}

	checkpoints, err := h.ledger.ListCheckpoints(ctx, req.GetAccountId(), period, int(req.GetLimit()))
	if err != nil {
		return nil, h.status(ctx, err)
	}

	response := &ledgerv1.ListCheckpointsResponse{}
	for _, checkpoint := range checkpoints {
		response.Checkpoints = append(response.Checkpoints, checkpointToProto(checkpoint))
	}

	return response, nil
}

// status turns a service error into a gRPC status. Messages for the caller's own mistakes come
// from the domain and are safe to show; anything unexpected is logged and reported as a generic
// internal error so database details never leave the service.
func (h *Handler) status(ctx context.Context, err error) error {
	var insufficient *domain.InsufficientFundsError

	switch {
	case errors.Is(err, processmoney.ErrInvalidAmount), errors.Is(err, domain.ErrInvalidID), errors.Is(err, domain.ErrInvalidPeriod):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.As(err, &insufficient):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, domain.ErrTransactionConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request cancelled")
	default:
		h.log.ErrorContext(ctx, "ledger request failed", "error", err)

		return status.Error(codes.Internal, "internal error")
	}
}

func entryToProto(entry domain.Entry) *ledgerv1.Entry {
	return &ledgerv1.Entry{
		Id:              entry.ID,
		AccountId:       entry.AccountID,
		TransactionId:   entry.TransactionID,
		Seq:             entry.Seq,
		Direction:       directionToProto(entry.Direction),
		Amount:          processmoney.FromMinorUnits(entry.Amount),
		CreatedAtMicros: entry.CreatedAt.UnixMicro(),
	}
}

func checkpointToProto(checkpoint domain.Checkpoint) *ledgerv1.Checkpoint {
	return &ledgerv1.Checkpoint{
		Id:                checkpoint.ID,
		AccountId:         checkpoint.AccountID,
		Period:            periodToProto(checkpoint.Period),
		PeriodStartMicros: checkpoint.PeriodStart.UnixMicro(),
		PeriodEndMicros:   checkpoint.PeriodEnd.UnixMicro(),
		LastSeq:           checkpoint.LastSeq,
		Balance:           processmoney.FromMinorUnits(checkpoint.Balance),
		CreatedAtMicros:   checkpoint.CreatedAt.UnixMicro(),
	}
}

func directionToProto(direction domain.Direction) ledgerv1.Direction {
	switch direction {
	case domain.Debit:
		return ledgerv1.Direction_DIRECTION_DEBIT
	case domain.Credit:
		return ledgerv1.Direction_DIRECTION_CREDIT
	default:
		return ledgerv1.Direction_DIRECTION_UNSPECIFIED
	}
}

var periodsToProto = map[domain.Period]ledgerv1.Period{
	domain.PeriodMonth:    ledgerv1.Period_PERIOD_MONTH,
	domain.PeriodQuarter:  ledgerv1.Period_PERIOD_QUARTER,
	domain.PeriodYear:     ledgerv1.Period_PERIOD_YEAR,
	domain.PeriodFiveYear: ledgerv1.Period_PERIOD_FIVE_YEAR,
}

func periodToProto(period domain.Period) ledgerv1.Period {
	return periodsToProto[period]
}

// periodFromProto maps the request's period; unspecified means every period (an empty Period).
func periodFromProto(period ledgerv1.Period) (domain.Period, error) {
	if period == ledgerv1.Period_PERIOD_UNSPECIFIED {
		return "", nil
	}

	for domainPeriod, protoPeriod := range periodsToProto {
		if protoPeriod == period {
			return domainPeriod, nil
		}
	}

	return "", domain.ErrInvalidPeriod
}

// LoggingInterceptor logs one line per call: method, result code and duration. Request contents
// are not logged.
func LoggingInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		start := time.Now()
		response, err := next(ctx, req)

		log.InfoContext(ctx, "rpc",
			"method", info.FullMethod,
			"code", status.Code(err).String(),
			"duration_ms", time.Since(start).Milliseconds(),
		)

		return response, err
	}
}
