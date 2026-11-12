package application

import (
	"context"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// AccountView is an account with the figures a user cares about, in cents. Available is what can
// be spent now: Balance less Held (active holds) less operations still on their way out.
type AccountView struct {
	domain.Account
	Balance    int64
	Held       int64
	Available  int64
	SpentToday int64
}

// WalletView is a user's wallet with every account.
type WalletView struct {
	domain.Wallet
	SpentToday int64
	Accounts   []AccountView
}

// LimitChange is an optional change to a daily limit: leave it, set it, or clear it.
type LimitChange struct {
	Set   bool   // false leaves the limit alone
	Value *int64 // nil with Set clears the limit; otherwise the new limit in cents
}

// AccountPatch is a change to an account. Nil fields stay as they are.
type AccountPatch struct {
	Name   *string
	Limit  LimitChange
	Status *domain.AccountStatus // only AccountActive (unfreeze) and AccountFrozen
}

// UserMoneyRequest is a deposit or a withdrawal.
type UserMoneyRequest struct {
	UserID         string
	AccountID      string
	Amount         int64 // cents
	IdempotencyKey string
	OTP            string
}

// TransferRequest is a transfer from one of the user's accounts to any account.
type TransferRequest struct {
	UserID         string
	FromAccountID  string
	ToAccountID    string
	Amount         int64 // cents
	IdempotencyKey string
	OTP            string
}

// ServiceMoneyRequest is a credit, debit or transfer asked for by another service.
type ServiceMoneyRequest struct {
	Service        string // who is asking, e.g. "payment"
	AccountID      string // the account credited or debited; a transfer's source
	CounterpartyID string // a transfer's destination
	Amount         int64  // cents
	HoldID         string // a hold to capture, for a debit or transfer
	IdempotencyKey string
}

// CreateHoldRequest reserves money on an account.
type CreateHoldRequest struct {
	Service        string
	AccountID      string
	Amount         int64 // cents
	Reason         string
	TTL            time.Duration
	IdempotencyKey string
}

// ConfirmRequest finishes an over-limit operation.
type ConfirmRequest struct {
	UserID       string
	OperationID  string
	OTP          string
	Acknowledged bool // the user explicitly accepts going over the limit
}

// Wallets is what the user-facing adapters (REST) may ask of the core. Every call is scoped to
// the authenticated user: another user's account or operation is domain.ErrNotFound.
type Wallets interface {
	Authenticate(ctx context.Context, bearerToken string) (userID string, err error)

	Wallet(ctx context.Context, userID string) (WalletView, error)
	SetWalletLimit(ctx context.Context, userID, otp string, change LimitChange) (WalletView, error)

	CreateAccount(ctx context.Context, userID, name string, limit *int64) (AccountView, error)
	Account(ctx context.Context, userID, accountID string) (AccountView, error)
	UpdateAccount(ctx context.Context, userID, accountID, otp string, patch AccountPatch) (AccountView, error)
	CloseAccount(ctx context.Context, userID, accountID, otp string) (AccountView, error)
	Transactions(ctx context.Context, userID, accountID string, before *OperationCursor, limit int) ([]domain.Operation, error)

	Deposit(ctx context.Context, req UserMoneyRequest) (domain.Operation, error)
	Withdraw(ctx context.Context, req UserMoneyRequest) (domain.Operation, error)
	Transfer(ctx context.Context, req TransferRequest) (domain.Operation, error)

	Operation(ctx context.Context, userID, operationID string) (domain.Operation, error)
	Confirm(ctx context.Context, req ConfirmRequest) (domain.Operation, error)
	Cancel(ctx context.Context, userID, operationID string) (domain.Operation, error)
}

// Internal is what the internal gRPC adapter may ask of the core, for other services.
//
// Credit, Debit and Transfer return the operation however far it got: it may still be pending if
// the ledger was unreachable, and finishes in the background. When a spend would exceed a limit
// they return the saved operation together with a *domain.ConfirmationRequiredError.
type Internal interface {
	Credit(ctx context.Context, req ServiceMoneyRequest) (domain.Operation, error)
	Debit(ctx context.Context, req ServiceMoneyRequest) (domain.Operation, error)
	TransferBetween(ctx context.Context, req ServiceMoneyRequest) (domain.Operation, error)
	CreateHold(ctx context.Context, req CreateHoldRequest) (domain.Hold, error)
	ReleaseHold(ctx context.Context, service, holdID string) (domain.Hold, error)
	AccountFor(ctx context.Context, accountID string) (AccountView, error)
	OperationFor(ctx context.Context, operationID string) (domain.Operation, error)
}

// RecoveryReport says what one recovery pass did.
type RecoveryReport struct {
	Advanced int // operations picked up and driven
	Expired  int // unconfirmed operations whose window closed
}

// Recovery is what the recovery adapter asks of the core on every tick.
type Recovery interface {
	Recover(ctx context.Context) (RecoveryReport, error)
}
