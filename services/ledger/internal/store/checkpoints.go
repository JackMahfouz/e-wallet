package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/jackc/pgx/v5"
)

// ListCheckpoints implements domain.Repository. Newest first; for checkpoints that close at the
// same instant (a month, a quarter and a year all ending on 31 December) the finest period comes first.
func (s *Store) ListCheckpoints(ctx context.Context, accountID string, period domain.Period, limit int) ([]domain.Checkpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, account_id::text, period, period_start, period_end, last_seq, balance, created_at
		 FROM ledger_checkpoints
		 WHERE account_id = $1 AND ($2::text = '' OR period = $2::text)
		 ORDER BY period_end DESC, period_start DESC
		 LIMIT $3`,
		accountID, string(period), limit)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer rows.Close()

	var checkpoints []domain.Checkpoint
	for rows.Next() {
		var (
			checkpoint domain.Checkpoint
			periodName string
		)
		if err := rows.Scan(&checkpoint.ID, &checkpoint.AccountID, &periodName, &checkpoint.PeriodStart,
			&checkpoint.PeriodEnd, &checkpoint.LastSeq, &checkpoint.Balance, &checkpoint.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		checkpoint.Period = domain.Period(periodName)
		checkpoint.PeriodStart = checkpoint.PeriodStart.UTC()
		checkpoint.PeriodEnd = checkpoint.PeriodEnd.UTC()
		checkpoint.CreatedAt = checkpoint.CreatedAt.UTC()
		checkpoints = append(checkpoints, checkpoint)
	}

	return checkpoints, rows.Err()
}

// AccountsPendingCheckpoint implements domain.Repository.
//
// It scans the whole entries table, grouped by account, so its cost grows with the ledger. That is
// fine for a periodic background job; if it ever is not, track accounts with new entries instead.
func (s *Store) AccountsPendingCheckpoint(ctx context.Context, period domain.Period) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT e.account_id::text
		 FROM (SELECT account_id, MAX(seq) AS max_seq FROM ledger_entries GROUP BY account_id) e
		 LEFT JOIN LATERAL (
		     SELECT last_seq FROM ledger_checkpoints c
		     WHERE c.account_id = e.account_id AND c.period = $1
		     ORDER BY c.last_seq DESC
		     LIMIT 1
		 ) latest ON true
		 WHERE e.max_seq > COALESCE(latest.last_seq, 0)`,
		string(period))
	if err != nil {
		return nil, fmt.Errorf("list accounts pending a checkpoint: %w", err)
	}
	defer rows.Close()

	var accounts []string
	for rows.Next() {
		var accountID string
		if err := rows.Scan(&accountID); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, accountID)
	}

	return accounts, rows.Err()
}

// AppendNextCheckpoint implements domain.Repository.
//
// Each period type keeps its own chain of checkpoints: a checkpoint is the previous checkpoint of
// the same period plus the entries since, never derived from a finer period's checkpoint. So the
// four chains are independent calculations of the same balances, and a mistake in one cannot
// hide in another.
//
// Only periods in which the account had entries get a checkpoint; quiet periods are skipped, and
// the next checkpoint simply covers everything since the last one.
func (s *Store) AppendNextCheckpoint(ctx context.Context, accountID string, period domain.Period, closedBefore time.Time) (bool, error) {
	var appended bool

	err := s.inLockedTx(ctx, accountID, func(tx pgx.Tx) error {
		// Where this period's chain stands: nothing yet, or the last checkpoint.
		var (
			previousSeq, previousBalance int64
			previousEnd                  time.Time
		)
		err := tx.QueryRow(ctx,
			`SELECT last_seq, balance, period_end FROM ledger_checkpoints
			 WHERE account_id = $1 AND period = $2
			 ORDER BY last_seq DESC LIMIT 1`,
			accountID, string(period)).Scan(&previousSeq, &previousBalance, &previousEnd)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read latest checkpoint: %w", err)
		}

		// The next entry that is not covered yet decides which period to close.
		var nextEntryAt time.Time
		err = tx.QueryRow(ctx,
			`SELECT created_at FROM ledger_entries
			 WHERE account_id = $1 AND seq > $2
			 ORDER BY seq LIMIT 1`,
			accountID, previousSeq).Scan(&nextEntryAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read next entry: %w", err)
		}

		// If the clock ever ran backwards, an entry can be older than the checkpoint before it;
		// it then belongs to the next period rather than to one that is already closed.
		if nextEntryAt.Before(previousEnd) {
			nextEntryAt = previousEnd
		}

		periodStart, periodEnd := period.Bounds(nextEntryAt)
		if periodEnd.After(closedBefore) {
			return nil
		}

		// Cover everything up to the end of that period. The timestamp only picks the cut point (the
		// last entry stamped before the period ended); the balance is then summed over the whole
		// sequence range up to it. That way a checkpoint always equals "the entries up to last_seq",
		// even if the clock ever stepped backwards and left an entry in that range stamped late:
		// summing by timestamp as well would skip it, and the tail after last_seq would never
		// count it either. The account lock means no entry can still be in flight, and any entry
		// appended later is stamped after the period ended.
		var lastSeq, net int64
		err = tx.QueryRow(ctx,
			`WITH cut AS (
			     SELECT COALESCE(MAX(seq), 0) AS last_seq
			     FROM ledger_entries
			     WHERE account_id = $1 AND seq > $2 AND created_at < $3
			 )
			 SELECT (SELECT last_seq FROM cut)::bigint,
			        COALESCE(SUM(`+signedAmount+`), 0)::bigint
			 FROM ledger_entries
			 WHERE account_id = $1 AND seq > $2 AND seq <= (SELECT last_seq FROM cut)`,
			accountID, previousSeq, periodEnd).Scan(&lastSeq, &net)
		if err != nil {
			return fmt.Errorf("sum entries in period: %w", err)
		}
		if lastSeq == 0 {
			return nil
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_checkpoints (account_id, period, period_start, period_end, last_seq, balance)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			accountID, string(period), periodStart, periodEnd, lastSeq, previousBalance+net); err != nil {
			return fmt.Errorf("append checkpoint: %w", err)
		}

		appended = true

		return nil
	})

	return appended, err
}
