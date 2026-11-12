package domain

import (
	"errors"
	"fmt"
	"time"
)

// The errors callers can tell apart. Adapters map them to status codes; the core never knows how.
var (
	ErrNotFound = errors.New("not found")
	// ErrInvalid is wrapped by every input validation failure.
	ErrInvalid = errors.New("invalid input")
	// ErrConflict means the request is valid but the current state forbids it.
	ErrConflict = errors.New("conflict with the current state")
	// ErrAccountNotActive means an account is frozen or closed.
	ErrAccountNotActive = errors.New("account is not active")
	// ErrIdempotencyKeyReused means a key was already used for a different request.
	ErrIdempotencyKeyReused = errors.New("idempotency key already used for a different request")
	// ErrDuplicateKey is what a repository returns when an operation or hold with the same
	// (initiator, idempotency key) already exists.
	ErrDuplicateKey = errors.New("duplicate idempotency key")
	// ErrOTPInvalid means a one-time code is missing or wrong.
	ErrOTPInvalid = errors.New("one-time code missing or wrong")
	// ErrUnauthenticated means the bearer token is missing or not valid.
	ErrUnauthenticated = errors.New("not authenticated")
	// ErrFundingDisabled means deposits and withdrawals are switched off.
	ErrFundingDisabled = errors.New("funding is disabled")
	// ErrUnavailable means a dependency (ledger, auth service, funding) could not be reached.
	// It is transient: the operation is kept and tried again.
	ErrUnavailable = errors.New("dependency unavailable")
	// ErrRejected means a dependency refused the request for good (the ledger, or the funding
	// gateway). It is not transient.
	ErrRejected = errors.New("dependency rejected the request")
	// ErrLimitSum means the account limits would add up to more than the wallet limit.
	ErrLimitSum = errors.New("account limits would exceed the wallet limit")
)

// InsufficientFundsError is returned when an amount is more than can be spent right now.
type InsufficientFundsError struct {
	Available int64 // cents
	Requested int64 // cents
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient funds: available %d, requested %d (cents)", e.Available, e.Requested)
}

// LimitScope says which daily limit an operation would exceed. Zero means none.
type LimitScope int16

const (
	ScopeAccount LimitScope = 1
	ScopeWallet  LimitScope = 2
)

// LimitScopes lists every valid scope.
var LimitScopes = []LimitScope{ScopeAccount, ScopeWallet}

func (s LimitScope) String() string {
	switch s {
	case ScopeAccount:
		return "account"
	case ScopeWallet:
		return "wallet"
	default:
		return fmt.Sprintf("limit_scope(%d)", int16(s))
	}
}

// Valid reports whether s is a known code.
func (s LimitScope) Valid() bool { return s == ScopeAccount || s == ScopeWallet }

// ConfirmationRequiredError is returned, with the saved operation, when a spend would exceed a
// daily limit. Nothing has moved; the user confirms (or cancels) the operation within the TTL.
type ConfirmationRequiredError struct {
	OperationID string
	Scope       LimitScope
	ExpiresAt   time.Time
}

func (e *ConfirmationRequiredError) Error() string {
	return fmt.Sprintf("this would exceed the %s daily limit: confirm operation %s before %s",
		e.Scope, e.OperationID, e.ExpiresAt.UTC().Format(time.RFC3339))
}

// Invalidf returns an error wrapping ErrInvalid.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
