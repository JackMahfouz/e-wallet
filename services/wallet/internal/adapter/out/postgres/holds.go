package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/jackc/pgx/v5"
)

const holdColumns = `id::text, account_id::text, amount, reason, expires_at, released_at, captured_by::text,
	initiator, idempotency_key::text, request_hash, created_at`

func scanHold(row pgx.Row) (domain.Hold, error) {
	var (
		h        domain.Hold
		captured *string
	)

	err := row.Scan(&h.ID, &h.AccountID, &h.Amount, &h.Reason, &h.ExpiresAt, &h.ReleasedAt, &captured,
		&h.Initiator, &h.IdempotencyKey, &h.RequestHash, &h.CreatedAt)
	h.CapturedBy = deref(captured)
	h.ExpiresAt, h.ReleasedAt, h.CreatedAt = utc(h.ExpiresAt), utcPtr(h.ReleasedAt), utc(h.CreatedAt)

	return h, err
}

// InsertHold implements application.Repository.
func (r *repo) InsertHold(ctx context.Context, h domain.Hold) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO holds (id, account_id, amount, reason, expires_at, released_at, captured_by,
			initiator, idempotency_key, request_hash, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		h.ID, h.AccountID, h.Amount, h.Reason, utc(h.ExpiresAt), utcPtr(h.ReleasedAt), nullable(h.CapturedBy),
		h.Initiator, h.IdempotencyKey, h.RequestHash, utc(h.CreatedAt))
	if duplicate(err, holdsKey) {
		return domain.ErrDuplicateKey
	}
	if err != nil {
		return fmt.Errorf("insert hold: %w", err)
	}

	return nil
}

func (r *repo) oneHold(ctx context.Context, doing, where string, args ...any) (domain.Hold, error) {
	h, err := scanHold(r.q.QueryRow(ctx, `SELECT `+holdColumns+` FROM holds WHERE `+where, args...))
	if err != nil {
		return domain.Hold{}, notFound(err, doing)
	}

	return h, nil
}

// HoldByID implements application.Repository.
func (r *repo) HoldByID(ctx context.Context, id string) (domain.Hold, error) {
	return r.oneHold(ctx, "hold by id", `id = $1`, id)
}

// HoldByKey implements application.Repository.
func (r *repo) HoldByKey(ctx context.Context, initiator, key string) (domain.Hold, error) {
	return r.oneHold(ctx, "hold by key", `initiator = $1 AND idempotency_key = $2`, initiator, key)
}

// LockHold implements application.Repository.
func (r *repo) LockHold(ctx context.Context, id string) (domain.Hold, error) {
	return r.oneHold(ctx, "lock hold", `id = $1 FOR UPDATE`, id)
}

// SaveHold implements application.Repository.
func (r *repo) SaveHold(ctx context.Context, h domain.Hold) error {
	tag, err := r.q.Exec(ctx, `UPDATE holds SET released_at = $2, captured_by = $3 WHERE id = $1`,
		h.ID, utcPtr(h.ReleasedAt), nullable(h.CapturedBy))
	if err != nil {
		return fmt.Errorf("save hold: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// ActiveHeld implements application.Repository.
func (r *repo) ActiveHeld(ctx context.Context, accountID string, now time.Time) (int64, error) {
	return r.sum(ctx, "active held",
		`SELECT COALESCE(SUM(amount), 0)::bigint FROM holds
		 WHERE account_id = $1 AND released_at IS NULL AND captured_by IS NULL AND expires_at > $2`,
		accountID, utc(now))
}

// ActiveHoldCount implements application.Repository.
func (r *repo) ActiveHoldCount(ctx context.Context, accountID string, now time.Time) (int, error) {
	n, err := r.sum(ctx, "active hold count",
		`SELECT COUNT(*) FROM holds
		 WHERE account_id = $1 AND released_at IS NULL AND captured_by IS NULL AND expires_at > $2`,
		accountID, utc(now))

	return int(n), err
}

// ReleaseCapture implements application.Repository.
func (r *repo) ReleaseCapture(ctx context.Context, operationID string) error {
	if _, err := r.q.Exec(ctx, `UPDATE holds SET captured_by = NULL WHERE captured_by = $1`, operationID); err != nil {
		return fmt.Errorf("release capture: %w", err)
	}

	return nil
}

// prefixed puts alias. in front of every column in a comma-separated list, for RETURNING and
// joins. The list may carry casts (id::text), which stay after the column.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}

	return strings.Join(parts, ", ")
}
