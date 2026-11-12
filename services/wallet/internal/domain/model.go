package domain

import (
	"strings"
	"time"
	"unicode"
)

const maxNameLength = 64

// Wallet belongs to one user and holds the user's accounts and the global daily limit.
type Wallet struct {
	ID         string
	UserID     string
	DailyLimit *int64 // cents; nil means no limit
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Account is one of a wallet's accounts. Its money lives in the ledger, under the account's id.
type Account struct {
	ID         string
	WalletID   string
	Name       string
	Status     AccountStatus
	DailyLimit *int64 // cents; nil means no limit of its own
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ClosedAt   *time.Time
}

// CanMove reports whether the account may send or receive money.
func (a Account) CanMove() bool { return a.Status == AccountActive }

// ParseName trims and checks an account name.
func ParseName(name string) (string, error) {
	name = strings.TrimSpace(name)

	switch {
	case name == "":
		return "", Invalidf("name must not be empty")
	case len([]rune(name)) > maxNameLength:
		return "", Invalidf("name must be at most %d characters", maxNameLength)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", Invalidf("name must not contain control characters")
		}
	}

	return name, nil
}

// ParseLimit checks a daily limit in cents. Limits are positive; "no limit" is nil, not zero.
func ParseLimit(cents int64) (int64, error) {
	if cents <= 0 {
		return 0, Invalidf("a limit must be greater than 0 (clear it to remove the limit)")
	}

	return cents, nil
}

// Operation is one journal entry: a request to move money, and how far it has got. Its id is also
// the ledger transaction id of every ledger call it makes, which is what makes each call safe to
// repeat.
type Operation struct {
	ID   string
	Kind OperationKind

	// AccountID is the account the operation is about: the one money leaves (debit, withdrawal,
	// transfer) or arrives at (credit, deposit). CounterpartyID is a transfer's destination.
	AccountID      string
	CounterpartyID string

	Amount int64  // cents
	HoldID string // a hold this debit or transfer captures, if any

	Status  OperationStatus
	Failure FailureReason // zero unless Status is StatusFailed

	// Initiator is "user:<uuid>" or "service:<name>".
	Initiator      string
	IdempotencyKey string
	RequestHash    string

	// Spending marks operations that count against the daily limits: money leaving the wallet.
	Spending           bool
	LimitScope         LimitScope // which limit it would exceed, while awaiting confirmation
	OverLimitConfirmed bool
	ConfirmExpiresAt   *time.Time

	// DeadlineAt is when an operation that has not succeeded is failed: five working days on.
	DeadlineAt time.Time

	Attempts      int
	NextAttemptAt time.Time

	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExecutedAt  *time.Time // when it became Pending: what the daily limits count from
	RefundedAt  *time.Time
	CompletedAt *time.Time
}

// Touches reports whether the operation involves the account, on either side.
func (o Operation) Touches(accountID string) bool {
	return o.AccountID == accountID || (o.CounterpartyID != "" && o.CounterpartyID == accountID)
}

// Expired reports whether an awaiting operation's confirmation window has closed.
func (o Operation) Expired(now time.Time) bool {
	return o.Status == StatusAwaitingConfirmation && o.ConfirmExpiresAt != nil && !now.Before(*o.ConfirmExpiresAt)
}

// PastDeadline reports whether the operation has run out of time.
func (o Operation) PastDeadline(now time.Time) bool { return !now.Before(o.DeadlineAt) }

// Confirm moves an awaiting operation to Pending. The caller has already checked that the user
// acknowledged the limit and that the window is still open.
func (o *Operation) Confirm(now time.Time) error {
	if o.Status != StatusAwaitingConfirmation {
		return Invalidf("operation is %s, not awaiting confirmation", o.Status)
	}

	o.Status = StatusPending
	o.OverLimitConfirmed = true
	o.LimitScope = 0
	o.ConfirmExpiresAt = nil
	o.ExecutedAt = &now
	o.UpdatedAt = now

	return nil
}

// Settle records that the first leg is done: money has left its source.
func (o *Operation) Settle(now time.Time) error {
	if o.Status != StatusPending {
		return Invalidf("operation is %s, not pending", o.Status)
	}

	o.Status = StatusSettling
	o.UpdatedAt = now

	return nil
}

// Complete records that the money is where it should be.
func (o *Operation) Complete(now time.Time) error {
	if o.Status != StatusPending && o.Status != StatusSettling {
		return Invalidf("operation is %s, so it cannot complete", o.Status)
	}

	o.Status = StatusCompleted
	o.CompletedAt = &now
	o.UpdatedAt = now

	return nil
}

// Fail ends an operation that did not happen, or whose effects were refunded.
func (o *Operation) Fail(reason FailureReason, now time.Time) error {
	if o.Status.Terminal() {
		return Invalidf("operation is already %s", o.Status)
	}
	if !reason.Valid() {
		return Invalidf("unknown failure reason %d", int16(reason))
	}

	o.Status = StatusFailed
	o.Failure = reason
	o.ConfirmExpiresAt = nil
	o.CompletedAt = &now
	o.UpdatedAt = now

	return nil
}

// Hold reserves part of an account's money for a payment that is not final yet. While active it
// is subtracted from what the account can spend; capturing it (a debit or transfer that names it)
// or releasing it, or letting it expire, ends the reservation.
type Hold struct {
	ID        string
	AccountID string
	Amount    int64 // cents
	Reason    string
	ExpiresAt time.Time

	ReleasedAt *time.Time
	// CapturedBy is the operation that consumed the hold, if one did.
	CapturedBy string

	Initiator      string
	IdempotencyKey string
	RequestHash    string
	CreatedAt      time.Time
}

// Active reports whether the hold still reserves money at now.
func (h Hold) Active(now time.Time) bool {
	return h.ReleasedAt == nil && h.CapturedBy == "" && now.Before(h.ExpiresAt)
}
