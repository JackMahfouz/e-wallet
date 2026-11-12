// Package application holds the wallet's use cases and the ports they talk through.
//
// Hexagonal architecture: this package and domain are the hexagon. Everything outside (HTTP, gRPC,
// PostgreSQL, the ledger, the auth service, OTP, funding) is reached only through the interfaces
// in this package, which the adapters implement. Nothing here imports a database, HTTP or gRPC
// package; an architecture test enforces that.
package application

import (
	"context"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// Clock tells the time. The core never calls time.Now, so tests control it exactly.
type Clock interface {
	Now() time.Time
}

// IDGenerator makes new UUIDs.
type IDGenerator interface {
	NewID() string
}

// TokenVerifier turns a bearer token into the user it was issued to.
//
// Errors: domain.ErrUnauthenticated for a token that is not valid, domain.ErrUnavailable when the
// auth service cannot be reached.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (userID string, err error)
}

// OTPVerifier checks the one-time code a user sent to confirm a sensitive action. The static
// implementation stands in until libs/auth issues real codes.
//
// Errors: domain.ErrOTPInvalid for a missing or wrong code.
type OTPVerifier interface {
	Verify(ctx context.Context, userID, code string) error
}

// FundingGateway moves money between a user's account and the outside world (a card, a bank).
// Both calls must be idempotent in reference: repeating one with the same reference does not move
// money twice.
//
// Errors: domain.ErrRejected for a refusal that will not change, domain.ErrUnavailable for a
// failure that may pass.
type FundingGateway interface {
	// Collect takes money from the outside world, to be credited to an account.
	Collect(ctx context.Context, reference string, cents int64) error
	// Payout sends money from an account to the outside world, after it has been debited.
	Payout(ctx context.Context, reference string, cents int64) error
}

// Ledger is the record of every account's money. transactionID is always the operation's id, and
// the ledger applies each (account, transaction, direction) once, so every call is safe to repeat.
//
// Errors: *domain.InsufficientFundsError when a debit exceeds the balance, domain.ErrRejected for
// a request the ledger will never accept, domain.ErrUnavailable when it cannot be reached.
type Ledger interface {
	Balance(ctx context.Context, accountID string) (cents int64, err error)
	Debit(ctx context.Context, accountID, transactionID string, cents int64) (LedgerPosting, error)
	Credit(ctx context.Context, accountID, transactionID string, cents int64) (LedgerPosting, error)
}

// LedgerPosting is what the ledger says about a debit or credit.
type LedgerPosting struct {
	// Replayed is true when the transaction had been posted before and nothing new was appended.
	Replayed bool
}

// OperationCursor is a position in an account's operation list, newest first.
type OperationCursor struct {
	CreatedAt time.Time
	ID        string
}

// Store is the wallet's persistence: the repository for single calls, and Do for a group of calls
// that must succeed or fail together.
type Store interface {
	Repository

	// Do runs fn in one transaction: the Repository it is given sees its own writes, and they all
	// commit if fn returns nil and are all discarded if it returns an error.
	Do(ctx context.Context, fn func(ctx context.Context, repo Repository) error) error
}

// Repository reads and writes wallets, accounts, operations and holds. Lookups return
// domain.ErrNotFound when nothing matches. The Lock methods take row locks that last until the
// surrounding Do ends; outside Do they are plain reads.
//
// Lock order, which every use case follows so that none can deadlock: the wallet first, then
// accounts in ascending id order, then holds and operations.
type Repository interface {
	// EnsureWallet inserts w unless its user already has a wallet, and returns the user's wallet.
	EnsureWallet(ctx context.Context, w domain.Wallet) (domain.Wallet, error)
	WalletByUser(ctx context.Context, userID string) (domain.Wallet, error)
	WalletByID(ctx context.Context, id string) (domain.Wallet, error)
	LockWallet(ctx context.Context, id string) (domain.Wallet, error)
	SaveWallet(ctx context.Context, w domain.Wallet) error

	InsertAccount(ctx context.Context, a domain.Account) error
	AccountByID(ctx context.Context, id string) (domain.Account, error)
	// LockAccounts locks the accounts in ascending id order and returns them by id.
	LockAccounts(ctx context.Context, ids ...string) (map[string]domain.Account, error)
	AccountsByWallet(ctx context.Context, walletID string) ([]domain.Account, error)
	SaveAccount(ctx context.Context, a domain.Account) error

	// InsertOperation returns domain.ErrDuplicateKey if the (initiator, idempotency key) is taken.
	InsertOperation(ctx context.Context, op domain.Operation) error
	OperationByID(ctx context.Context, id string) (domain.Operation, error)
	OperationByKey(ctx context.Context, initiator, key string) (domain.Operation, error)
	LockOperation(ctx context.Context, id string) (domain.Operation, error)
	SaveOperation(ctx context.Context, op domain.Operation) error
	// ListAccountOperations returns operations touching the account, newest first, those strictly
	// after before in that order.
	ListAccountOperations(ctx context.Context, accountID string, before *OperationCursor, limit int) ([]domain.Operation, error)
	// SpentSince sums the spending operations executed from since on (pending, settling or
	// completed) whose money leaves the account.
	SpentSince(ctx context.Context, accountID string, since time.Time) (int64, error)
	// WalletSpentSince is SpentSince across all of a wallet's accounts.
	WalletSpentSince(ctx context.Context, walletID string, since time.Time) (int64, error)
	// PendingOutgoing sums the account's outgoing operations whose debit has not reached the
	// ledger yet (status pending).
	PendingOutgoing(ctx context.Context, accountID string) (int64, error)
	// InFlightCount counts operations touching the account, on either side, that are awaiting
	// confirmation, pending or settling.
	InFlightCount(ctx context.Context, accountID string) (int, error)
	// ClaimDue picks pending and settling operations whose next attempt is due, pushes their next
	// attempt lease on, counts the attempt, and returns them. Several wallet instances calling it
	// at once never get the same operation.
	ClaimDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Operation, error)
	// DueConfirmations lists awaiting operations whose confirmation window has closed.
	DueConfirmations(ctx context.Context, now time.Time, limit int) ([]domain.Operation, error)

	InsertHold(ctx context.Context, h domain.Hold) error
	HoldByID(ctx context.Context, id string) (domain.Hold, error)
	HoldByKey(ctx context.Context, initiator, key string) (domain.Hold, error)
	LockHold(ctx context.Context, id string) (domain.Hold, error)
	SaveHold(ctx context.Context, h domain.Hold) error
	// ActiveHeld sums the account's holds that are still active at now.
	ActiveHeld(ctx context.Context, accountID string, now time.Time) (int64, error)
	ActiveHoldCount(ctx context.Context, accountID string, now time.Time) (int, error)
	// ReleaseCapture undoes the capture of the hold the operation took, if it took one.
	ReleaseCapture(ctx context.Context, operationID string) error
}
