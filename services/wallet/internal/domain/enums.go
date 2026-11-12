// Package domain is the wallet's vocabulary and rules: wallets, accounts, operations, holds,
// limits and the working-day calendar. It does no I/O and imports nothing but the standard
// library, so everything here is testable without a database, a network or a clock.
package domain

import "fmt"

// Enums are stored as smallint columns and travel as these explicit codes, never as an ordinal
// that a reordering of the constants could change. The codes are part of the schema: see the
// CHECK constraints in migrations/001_create_wallet.sql, and the test that keeps the two in step.

// AccountStatus says whether an account may move money.
type AccountStatus int16

const (
	AccountActive AccountStatus = 1
	AccountFrozen AccountStatus = 2
	AccountClosed AccountStatus = 3 // terminal
)

// AccountStatuses lists every valid status.
var AccountStatuses = []AccountStatus{AccountActive, AccountFrozen, AccountClosed}

func (s AccountStatus) String() string {
	switch s {
	case AccountActive:
		return "active"
	case AccountFrozen:
		return "frozen"
	case AccountClosed:
		return "closed"
	default:
		return fmt.Sprintf("account_status(%d)", int16(s))
	}
}

// Valid reports whether s is a known code.
func (s AccountStatus) Valid() bool { return s >= AccountActive && s <= AccountClosed }

// OperationKind is what an operation does to money.
type OperationKind int16

const (
	// KindCredit and KindDebit are service-initiated (Payment) and touch one account.
	KindCredit OperationKind = 1
	KindDebit  OperationKind = 2
	// KindTransfer moves money between two accounts, of the same wallet or of two.
	KindTransfer OperationKind = 3
	// KindDeposit and KindWithdrawal are user-initiated and go through the funding gateway.
	KindDeposit    OperationKind = 4
	KindWithdrawal OperationKind = 5
)

// OperationKinds lists every valid kind.
var OperationKinds = []OperationKind{KindCredit, KindDebit, KindTransfer, KindDeposit, KindWithdrawal}

func (k OperationKind) String() string {
	switch k {
	case KindCredit:
		return "credit"
	case KindDebit:
		return "debit"
	case KindTransfer:
		return "transfer"
	case KindDeposit:
		return "deposit"
	case KindWithdrawal:
		return "withdrawal"
	default:
		return fmt.Sprintf("operation_kind(%d)", int16(k))
	}
}

// Valid reports whether k is a known code.
func (k OperationKind) Valid() bool { return k >= KindCredit && k <= KindWithdrawal }

// MovesMoneyOut reports whether the first leg takes money from one of the wallet's accounts.
func (k OperationKind) MovesMoneyOut() bool {
	return k == KindDebit || k == KindTransfer || k == KindWithdrawal
}

// OperationStatus is where an operation is in its life.
//
//	AwaitingConfirmation --confirm--> Pending --first leg--> Settling --second leg--> Completed
//	        |                            |                       |
//	        +-- expire / cancel          +-- refuse / time out   +-- refund (money back) --> Failed
//	                    +---------------------> Failed <------------------------------------+
type OperationStatus int16

const (
	// StatusAwaitingConfirmation: over a limit; nothing has moved or been reserved.
	StatusAwaitingConfirmation OperationStatus = 1
	// StatusPending: accepted; no money has left its source yet.
	StatusPending OperationStatus = 2
	// StatusSettling: money has left its source (the account was debited, or the funding source
	// was collected from) but has not arrived at its destination yet.
	StatusSettling OperationStatus = 3
	// StatusCompleted: money is where it should be. Terminal.
	StatusCompleted OperationStatus = 4
	// StatusFailed: nothing happened, or whatever happened was refunded. Terminal.
	StatusFailed OperationStatus = 5
)

// OperationStatuses lists every valid status.
var OperationStatuses = []OperationStatus{
	StatusAwaitingConfirmation, StatusPending, StatusSettling, StatusCompleted, StatusFailed,
}

func (s OperationStatus) String() string {
	switch s {
	case StatusAwaitingConfirmation:
		return "awaiting_confirmation"
	case StatusPending:
		return "pending"
	case StatusSettling:
		return "settling"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	default:
		return fmt.Sprintf("operation_status(%d)", int16(s))
	}
}

// Valid reports whether s is a known code.
func (s OperationStatus) Valid() bool { return s >= StatusAwaitingConfirmation && s <= StatusFailed }

// Terminal reports whether the operation can no longer change.
func (s OperationStatus) Terminal() bool { return s == StatusCompleted || s == StatusFailed }

// InFlight reports whether the operation still touches its accounts' money or may yet do so.
func (s OperationStatus) InFlight() bool { return s.Valid() && !s.Terminal() }

// FailureReason says why an operation failed.
type FailureReason int16

const (
	FailureInsufficientFunds   FailureReason = 1
	FailureTimedOut            FailureReason = 2
	FailureConfirmationExpired FailureReason = 3
	FailureRejected            FailureReason = 4
	FailureCancelled           FailureReason = 5
)

// FailureReasons lists every valid reason.
var FailureReasons = []FailureReason{
	FailureInsufficientFunds, FailureTimedOut, FailureConfirmationExpired, FailureRejected, FailureCancelled,
}

func (r FailureReason) String() string {
	switch r {
	case FailureInsufficientFunds:
		return "insufficient_funds"
	case FailureTimedOut:
		return "timed_out"
	case FailureConfirmationExpired:
		return "confirmation_expired"
	case FailureRejected:
		return "rejected"
	case FailureCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("failure_reason(%d)", int16(r))
	}
}

// Valid reports whether r is a known code.
func (r FailureReason) Valid() bool { return r >= FailureInsufficientFunds && r <= FailureCancelled }
