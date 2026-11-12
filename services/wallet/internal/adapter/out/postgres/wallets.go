package postgres

import (
	"context"
	"fmt"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/jackc/pgx/v5"
)

const walletColumns = `id::text, user_id::text, daily_limit, created_at, updated_at`

func scanWallet(row pgx.Row) (domain.Wallet, error) {
	var w domain.Wallet

	err := row.Scan(&w.ID, &w.UserID, &w.DailyLimit, &w.CreatedAt, &w.UpdatedAt)
	w.CreatedAt, w.UpdatedAt = utc(w.CreatedAt), utc(w.UpdatedAt)

	return w, err
}

// EnsureWallet implements application.Repository.
func (r *repo) EnsureWallet(ctx context.Context, w domain.Wallet) (domain.Wallet, error) {
	if _, err := r.q.Exec(ctx,
		`INSERT INTO wallets (id, user_id, daily_limit, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (user_id) DO NOTHING`,
		w.ID, w.UserID, w.DailyLimit, utc(w.CreatedAt), utc(w.UpdatedAt)); err != nil {
		return domain.Wallet{}, fmt.Errorf("ensure wallet: %w", err)
	}

	return r.WalletByUser(ctx, w.UserID)
}

// WalletByUser implements application.Repository.
func (r *repo) WalletByUser(ctx context.Context, userID string) (domain.Wallet, error) {
	w, err := scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE user_id = $1`, userID))
	if err != nil {
		return domain.Wallet{}, notFound(err, "wallet by user")
	}

	return w, nil
}

// WalletByID implements application.Repository.
func (r *repo) WalletByID(ctx context.Context, id string) (domain.Wallet, error) {
	w, err := scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id))
	if err != nil {
		return domain.Wallet{}, notFound(err, "wallet by id")
	}

	return w, nil
}

// LockWallet implements application.Repository.
func (r *repo) LockWallet(ctx context.Context, id string) (domain.Wallet, error) {
	w, err := scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return domain.Wallet{}, notFound(err, "lock wallet")
	}

	return w, nil
}

// SaveWallet implements application.Repository.
func (r *repo) SaveWallet(ctx context.Context, w domain.Wallet) error {
	tag, err := r.q.Exec(ctx, `UPDATE wallets SET daily_limit = $2, updated_at = $3 WHERE id = $1`,
		w.ID, w.DailyLimit, utc(w.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save wallet: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}
