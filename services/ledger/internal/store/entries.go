package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/jackc/pgx/v5"
)

// signedAmount makes a balance a plain sum: credits add and debits subtract.
const signedAmount = `CASE direction WHEN 'credit' THEN amount ELSE -amount END`

// balanceSQL is the one definition of an account's balance: its latest checkpoint (the entry
// sequence it covers and the balance at that point) plus every entry after that sequence. With no
// checkpoint yet it is simply the sum of all the account's entries. All in one statement, so
// balance and last_seq come from the same snapshot.
const balanceSQL = `
WITH checkpoint AS (
    SELECT balance, last_seq
    FROM ledger_checkpoints
    WHERE account_id = $1
    ORDER BY last_seq DESC
    LIMIT 1
)
SELECT
    (COALESCE((SELECT balance FROM checkpoint), 0)
     + COALESCE((SELECT SUM(` + signedAmount + `)
                 FROM ledger_entries
                 WHERE account_id = $1
                   AND seq > COALESCE((SELECT last_seq FROM checkpoint), 0)), 0))::bigint,
    COALESCE((SELECT MAX(seq) FROM ledger_entries WHERE account_id = $1), 0)::bigint`

func queryBalance(ctx context.Context, q queryer, accountID string) (domain.AccountBalance, error) {
	var balance domain.AccountBalance

	if err := q.QueryRow(ctx, balanceSQL, accountID).Scan(&balance.Balance, &balance.LastSeq); err != nil {
		return domain.AccountBalance{}, fmt.Errorf("query balance: %w", err)
	}

	return balance, nil
}

// Balance implements domain.Repository.
func (s *Store) Balance(ctx context.Context, accountID string) (domain.AccountBalance, error) {
	return queryBalance(ctx, s.pool, accountID)
}

const entryColumns = `id::text, account_id::text, transaction_id::text, seq, direction, amount, created_at`

func scanEntry(row pgx.Row) (domain.Entry, error) {
	var (
		entry     domain.Entry
		direction string
	)

	err := row.Scan(&entry.ID, &entry.AccountID, &entry.TransactionID, &entry.Seq, &direction, &entry.Amount, &entry.CreatedAt)
	entry.Direction = domain.Direction(direction)
	entry.CreatedAt = entry.CreatedAt.UTC()

	return entry, err
}

func findEntry(ctx context.Context, q queryer, accountID, transactionID string, direction domain.Direction) (*domain.Entry, error) {
	entry, err := scanEntry(q.QueryRow(ctx,
		`SELECT `+entryColumns+` FROM ledger_entries
		 WHERE account_id = $1 AND transaction_id = $2 AND direction = $3`,
		accountID, transactionID, string(direction)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find entry: %w", err)
	}

	return &entry, nil
}

// appendEntry adds the next entry to the account's ledger. The sequence number is the account's
// current maximum plus one; the caller holds the account lock, so nothing else can append between
// reading the maximum and inserting, and the unique (account_id, seq) key would reject a fork anyway.
func appendEntry(ctx context.Context, q queryer, entry domain.NewEntry) (domain.Entry, error) {
	row := q.QueryRow(ctx,
		`INSERT INTO ledger_entries (account_id, seq, transaction_id, direction, amount)
		 VALUES ($1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM ledger_entries WHERE account_id = $1), $2, $3, $4)
		 RETURNING `+entryColumns,
		entry.AccountID, entry.TransactionID, string(entry.Direction), entry.Amount)

	appended, err := scanEntry(row)
	if err != nil {
		return domain.Entry{}, fmt.Errorf("append entry: %w", err)
	}

	return appended, nil
}

// ListEntries implements domain.Repository.
func (s *Store) ListEntries(ctx context.Context, accountID string, afterSeq int64, limit int) ([]domain.Entry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+entryColumns+` FROM ledger_entries
		 WHERE account_id = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`,
		accountID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()

	var entries []domain.Entry
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		entries = append(entries, entry)
	}

	return entries, rows.Err()
}
