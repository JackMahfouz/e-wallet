// Package service holds the ledger's business rules: debit and credit append entries, a debit is
// refused when it exceeds the balance, and a retried post is not applied twice.
package service

import (
	"context"
	"fmt"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
)

const (
	defaultLimit = 100
	maxLimit     = 1000
)

// Ledger is the ledger's service layer. All amounts here are whole cents: decimal amounts only
// exist at the gRPC edge.
type Ledger struct {
	repo domain.Repository
}

// New returns a Ledger over repo.
func New(repo domain.Repository) *Ledger {
	return &Ledger{repo: repo}
}

// PostInput is a debit or credit to append.
type PostInput struct {
	AccountID     string
	TransactionID string
	Amount        int64 // cents, greater than zero
}

// PostResult is what a debit or credit returns.
type PostResult struct {
	Entry domain.Entry
	// Balance is the account's balance in cents after the call (the current balance for a retry).
	Balance int64
	// Replayed is true when the transaction had already been posted and nothing was appended.
	Replayed bool
}

// Debit appends a debit entry. It is refused with *domain.InsufficientFundsError when the amount
// is more than the account's balance, which includes every debit against an empty account.
func (l *Ledger) Debit(ctx context.Context, in PostInput) (PostResult, error) {
	return l.post(ctx, domain.Debit, in)
}

// Credit appends a credit entry. The first credit to an unknown account id starts its ledger.
func (l *Ledger) Credit(ctx context.Context, in PostInput) (PostResult, error) {
	return l.post(ctx, domain.Credit, in)
}

// post is the whole write path. Inside one database transaction, holding the account's lock, it
// looks for an earlier post of the same transaction, checks the balance, and appends. The balance
// is read under the lock, so it cannot change between the check and the append: two concurrent
// debits can never both be approved against the same money.
func (l *Ledger) post(ctx context.Context, direction domain.Direction, in PostInput) (PostResult, error) {
	accountID, err := domain.ParseID("account_id", in.AccountID)
	if err != nil {
		return PostResult{}, err
	}
	transactionID, err := domain.ParseID("transaction_id", in.TransactionID)
	if err != nil {
		return PostResult{}, err
	}
	if in.Amount <= 0 {
		return PostResult{}, fmt.Errorf("%w: must be greater than 0", processmoney.ErrInvalidAmount)
	}

	var result PostResult

	err = l.repo.WithAccountLock(ctx, accountID, func(tx domain.AccountTx) error {
		balance, err := tx.Balance(ctx)
		if err != nil {
			return err
		}

		// A retry of an already-posted leg returns the original entry and changes nothing.
		existing, err := tx.FindEntry(ctx, transactionID, direction)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Amount != in.Amount {
				return domain.ErrTransactionConflict
			}
			result = PostResult{Entry: *existing, Balance: balance, Replayed: true}

			return nil
		}

		// The balance check: nothing can be debited from an empty account, and no debit may
		// exceed what the account holds.
		if direction == domain.Debit && balance < in.Amount {
			return &domain.InsufficientFundsError{Balance: balance, Requested: in.Amount}
		}

		entry, err := tx.Append(ctx, domain.NewEntry{
			AccountID:     accountID,
			TransactionID: transactionID,
			Direction:     direction,
			Amount:        in.Amount,
		})
		if err != nil {
			return err
		}

		result = PostResult{Entry: entry, Balance: balance + direction.Sign()*in.Amount}

		return nil
	})
	if err != nil {
		return PostResult{}, err
	}

	return result, nil
}

// Balance returns the account's balance in cents and the sequence number of its newest entry.
// An account with no entries has a balance of zero.
func (l *Ledger) Balance(ctx context.Context, accountID string) (domain.AccountBalance, error) {
	id, err := domain.ParseID("account_id", accountID)
	if err != nil {
		return domain.AccountBalance{}, err
	}

	return l.repo.Balance(ctx, id)
}

// ListEntries returns up to limit entries after afterSeq, oldest first, and whether more follow.
func (l *Ledger) ListEntries(ctx context.Context, accountID string, afterSeq int64, limit int) ([]domain.Entry, bool, error) {
	id, err := domain.ParseID("account_id", accountID)
	if err != nil {
		return nil, false, err
	}
	afterSeq = max(afterSeq, 0)
	limit = clampLimit(limit)

	// One extra row tells whether there is another page, without a second query.
	entries, err := l.repo.ListEntries(ctx, id, afterSeq, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}

	return entries, false, nil
}

// ListCheckpoints returns up to limit checkpoints, newest first. An empty period means all periods.
func (l *Ledger) ListCheckpoints(ctx context.Context, accountID string, period domain.Period, limit int) ([]domain.Checkpoint, error) {
	id, err := domain.ParseID("account_id", accountID)
	if err != nil {
		return nil, err
	}
	if period != "" {
		if period, err = domain.ParsePeriod(string(period)); err != nil {
			return nil, err
		}
	}

	return l.repo.ListCheckpoints(ctx, id, period, clampLimit(limit))
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultLimit
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}
