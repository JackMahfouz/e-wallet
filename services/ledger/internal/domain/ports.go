package domain

import (
	"context"
	"time"
)

// Repository is what the ledger needs from storage. The service depends on this interface and
// the PostgreSQL store implements it, so neither imports the other.
//
// The ledger is append-only: there is deliberately no method that updates or deletes anything.
type Repository interface {
	// WithAccountLock runs fn in one database transaction while holding an exclusive lock on the
	// account's ledger, so only one append (or checkpoint) per account is in flight at a time.
	// The transaction commits if fn returns nil and rolls back otherwise; fn's error is returned as is.
	WithAccountLock(ctx context.Context, accountID string, fn func(AccountTx) error) error

	// Balance returns the account's balance: its latest checkpoint plus the entries after it.
	Balance(ctx context.Context, accountID string) (AccountBalance, error)

	// ListEntries returns up to limit entries with a sequence number above afterSeq, oldest first.
	ListEntries(ctx context.Context, accountID string, afterSeq int64, limit int) ([]Entry, error)

	// ListCheckpoints returns up to limit checkpoints, newest first. An empty period means all.
	ListCheckpoints(ctx context.Context, accountID string, period Period, limit int) ([]Checkpoint, error)

	// AccountsPendingCheckpoint returns the accounts that have entries newer than their latest
	// checkpoint of the given period.
	AccountsPendingCheckpoint(ctx context.Context, period Period) ([]string, error)

	// AppendNextCheckpoint appends the account's next checkpoint of the given period, if its
	// period ended before closedBefore, and reports whether it appended one.
	AppendNextCheckpoint(ctx context.Context, accountID string, period Period, closedBefore time.Time) (bool, error)
}

// AccountTx is the account-locked transaction handed to WithAccountLock's callback.
type AccountTx interface {
	// Balance returns the account's balance in cents.
	Balance(ctx context.Context) (int64, error)

	// FindEntry returns the entry already posted for this transaction id and direction, or nil.
	FindEntry(ctx context.Context, transactionID string, direction Direction) (*Entry, error)

	// Append adds an entry at the end of the account's ledger and returns it.
	Append(ctx context.Context, entry NewEntry) (Entry, error)
}
