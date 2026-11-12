// Package postgres is the driven adapter that keeps the wallet in PostgreSQL. It implements
// application.Store, and is the only place that knows SQL.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the wallet's database. As a repository it runs each call on its own; Do runs a group
// of calls in one transaction.
type Store struct {
	repo
	pool *pgxpool.Pool
}

var _ application.Store = (*Store)(nil)

// New returns a Store over an already-migrated database.
func New(pool *pgxpool.Pool) *Store {
	return &Store{repo: repo{q: pool}, pool: pool}
}

// Do implements application.Store. The Repository it hands to fn is bound to the transaction.
func (s *Store) Do(ctx context.Context, fn func(context.Context, application.Repository) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(ctx, &repo{q: tx})
	})
}

// queryer is the part of pgx that a pool and a transaction share.
type queryer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// repo implements application.Repository over a pool or a transaction.
type repo struct {
	q queryer
}

var _ application.Repository = (*repo)(nil)

// Constraint names the adapter reacts to.
const (
	operationsKey = "operations_idempotency_key"
	holdsKey      = "holds_idempotency_key"
)

// duplicate reports whether err is a violation of the named unique constraint.
func duplicate(err error, constraint string) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// notFound turns "no rows" into domain.ErrNotFound and wraps anything else with what was being done.
func notFound(err error, doing string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}

	return fmt.Errorf("%s: %w", doing, err)
}

func utc(t time.Time) time.Time { return t.UTC() }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()

	return &u
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

func nullableCode(code int16) *int16 {
	if code == 0 {
		return nil
	}

	return &code
}
