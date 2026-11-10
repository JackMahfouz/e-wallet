// Package store is the PostgreSQL implementation of domain.Repository.
//
// The tables are append-only (see migrations/001_create_ledger.sql): this package only ever runs
// INSERT and SELECT against them.
package store

import (
	"context"
	"fmt"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps the ledger in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

var _ domain.Repository = (*Store)(nil)

// New returns a Store over an already-migrated database.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// queryer is the part of pgx that both a pool and a transaction provide.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// lockSQL takes a transaction-scoped advisory lock keyed on the account. Everything that writes
// to an account's ledger takes it first, so appends and checkpoints for one account never overlap
// and an account's balance cannot change between being checked and being used. The lock is
// released automatically when the transaction ends. A hash collision between two accounts only
// makes them wait for each other; it never lets two writers into the same account.
const lockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`

// WithAccountLock implements domain.Repository.
func (s *Store) WithAccountLock(ctx context.Context, accountID string, fn func(domain.AccountTx) error) error {
	return s.inLockedTx(ctx, accountID, func(tx pgx.Tx) error {
		return fn(&accountTx{tx: tx, accountID: accountID})
	})
}

func (s *Store) inLockedTx(ctx context.Context, accountID string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, lockSQL, accountID); err != nil {
			return fmt.Errorf("lock account ledger: %w", err)
		}

		return fn(tx)
	})
}

// accountTx is the locked transaction handed to the service.
type accountTx struct {
	tx        pgx.Tx
	accountID string
}

var _ domain.AccountTx = (*accountTx)(nil)

func (a *accountTx) Balance(ctx context.Context) (int64, error) {
	balance, err := queryBalance(ctx, a.tx, a.accountID)

	return balance.Balance, err
}

func (a *accountTx) FindEntry(ctx context.Context, transactionID string, direction domain.Direction) (*domain.Entry, error) {
	return findEntry(ctx, a.tx, a.accountID, transactionID, direction)
}

func (a *accountTx) Append(ctx context.Context, entry domain.NewEntry) (domain.Entry, error) {
	return appendEntry(ctx, a.tx, entry)
}
