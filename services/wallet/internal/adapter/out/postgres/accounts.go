package postgres

import (
	"context"
	"fmt"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/jackc/pgx/v5"
)

const accountColumns = `id::text, wallet_id::text, name, status, daily_limit, created_at, updated_at, closed_at`

func scanAccount(row pgx.Row) (domain.Account, error) {
	var (
		a      domain.Account
		status int16
	)

	err := row.Scan(&a.ID, &a.WalletID, &a.Name, &status, &a.DailyLimit, &a.CreatedAt, &a.UpdatedAt, &a.ClosedAt)
	a.Status = domain.AccountStatus(status)
	a.CreatedAt, a.UpdatedAt, a.ClosedAt = utc(a.CreatedAt), utc(a.UpdatedAt), utcPtr(a.ClosedAt)

	return a, err
}

// InsertAccount implements application.Repository.
func (r *repo) InsertAccount(ctx context.Context, a domain.Account) error {
	if _, err := r.q.Exec(ctx,
		`INSERT INTO accounts (id, wallet_id, name, status, daily_limit, created_at, updated_at, closed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.ID, a.WalletID, a.Name, int16(a.Status), a.DailyLimit, utc(a.CreatedAt), utc(a.UpdatedAt), utcPtr(a.ClosedAt)); err != nil {
		return fmt.Errorf("insert account: %w", err)
	}

	return nil
}

// AccountByID implements application.Repository.
func (r *repo) AccountByID(ctx context.Context, id string) (domain.Account, error) {
	a, err := scanAccount(r.q.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
	if err != nil {
		return domain.Account{}, notFound(err, "account by id")
	}

	return a, nil
}

// LockAccounts implements application.Repository. Locking in ascending id order, whatever order
// the caller names them in, is what keeps two transfers between the same pair of accounts from
// deadlocking.
func (r *repo) LockAccounts(ctx context.Context, ids ...string) (map[string]domain.Account, error) {
	rows, err := r.q.Query(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	defer rows.Close()

	out := map[string]domain.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}

	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, domain.ErrNotFound
		}
	}

	return out, nil
}

// AccountsByWallet implements application.Repository.
func (r *repo) AccountsByWallet(ctx context.Context, walletID string) ([]domain.Account, error) {
	rows, err := r.q.Query(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE wallet_id = $1 ORDER BY created_at, id`, walletID)
	if err != nil {
		return nil, fmt.Errorf("accounts by wallet: %w", err)
	}
	defer rows.Close()

	var out []domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, a)
	}

	return out, rows.Err()
}

// SaveAccount implements application.Repository.
func (r *repo) SaveAccount(ctx context.Context, a domain.Account) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE accounts SET name = $2, status = $3, daily_limit = $4, updated_at = $5, closed_at = $6 WHERE id = $1`,
		a.ID, a.Name, int16(a.Status), a.DailyLimit, utc(a.UpdatedAt), utcPtr(a.ClosedAt))
	if err != nil {
		return fmt.Errorf("save account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}
