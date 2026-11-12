package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/jackc/pgx/v5"
)

const operationColumns = `id::text, kind, account_id::text, counterparty_id::text, amount, hold_id::text,
	status, failure_reason, limit_scope, initiator, idempotency_key::text, request_hash, spending,
	over_limit_confirmed, confirm_expires_at, deadline_at, attempts, next_attempt_at, created_at,
	updated_at, executed_at, refunded_at, completed_at`

func scanOperation(row pgx.Row) (domain.Operation, error) {
	var (
		op                   domain.Operation
		kind, status         int16
		failure, scope       *int16
		counterparty, holdID *string
	)

	err := row.Scan(&op.ID, &kind, &op.AccountID, &counterparty, &op.Amount, &holdID,
		&status, &failure, &scope, &op.Initiator, &op.IdempotencyKey, &op.RequestHash, &op.Spending,
		&op.OverLimitConfirmed, &op.ConfirmExpiresAt, &op.DeadlineAt, &op.Attempts, &op.NextAttemptAt, &op.CreatedAt,
		&op.UpdatedAt, &op.ExecutedAt, &op.RefundedAt, &op.CompletedAt)

	op.Kind, op.Status = domain.OperationKind(kind), domain.OperationStatus(status)
	op.CounterpartyID, op.HoldID = deref(counterparty), deref(holdID)
	if failure != nil {
		op.Failure = domain.FailureReason(*failure)
	}
	if scope != nil {
		op.LimitScope = domain.LimitScope(*scope)
	}
	op.ConfirmExpiresAt, op.ExecutedAt = utcPtr(op.ConfirmExpiresAt), utcPtr(op.ExecutedAt)
	op.RefundedAt, op.CompletedAt = utcPtr(op.RefundedAt), utcPtr(op.CompletedAt)
	op.DeadlineAt, op.NextAttemptAt = utc(op.DeadlineAt), utc(op.NextAttemptAt)
	op.CreatedAt, op.UpdatedAt = utc(op.CreatedAt), utc(op.UpdatedAt)

	return op, err
}

// InsertOperation implements application.Repository.
func (r *repo) InsertOperation(ctx context.Context, op domain.Operation) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO operations (id, kind, account_id, counterparty_id, amount, hold_id, status, failure_reason,
			limit_scope, initiator, idempotency_key, request_hash, spending, over_limit_confirmed,
			confirm_expires_at, deadline_at, attempts, next_attempt_at, created_at, updated_at,
			executed_at, refunded_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		op.ID, int16(op.Kind), op.AccountID, nullable(op.CounterpartyID), op.Amount, nullable(op.HoldID),
		int16(op.Status), nullableCode(int16(op.Failure)), nullableCode(int16(op.LimitScope)), op.Initiator,
		op.IdempotencyKey, op.RequestHash, op.Spending, op.OverLimitConfirmed, utcPtr(op.ConfirmExpiresAt),
		utc(op.DeadlineAt), op.Attempts, utc(op.NextAttemptAt), utc(op.CreatedAt), utc(op.UpdatedAt),
		utcPtr(op.ExecutedAt), utcPtr(op.RefundedAt), utcPtr(op.CompletedAt))
	if duplicate(err, operationsKey) {
		return domain.ErrDuplicateKey
	}
	if err != nil {
		return fmt.Errorf("insert operation: %w", err)
	}

	return nil
}

func (r *repo) oneOperation(ctx context.Context, doing, where string, args ...any) (domain.Operation, error) {
	op, err := scanOperation(r.q.QueryRow(ctx, `SELECT `+operationColumns+` FROM operations WHERE `+where, args...))
	if err != nil {
		return domain.Operation{}, notFound(err, doing)
	}

	return op, nil
}

// OperationByID implements application.Repository.
func (r *repo) OperationByID(ctx context.Context, id string) (domain.Operation, error) {
	return r.oneOperation(ctx, "operation by id", `id = $1`, id)
}

// OperationByKey implements application.Repository.
func (r *repo) OperationByKey(ctx context.Context, initiator, key string) (domain.Operation, error) {
	return r.oneOperation(ctx, "operation by key", `initiator = $1 AND idempotency_key = $2`, initiator, key)
}

// LockOperation implements application.Repository.
func (r *repo) LockOperation(ctx context.Context, id string) (domain.Operation, error) {
	return r.oneOperation(ctx, "lock operation", `id = $1 FOR UPDATE`, id)
}

// SaveOperation implements application.Repository. It writes every column that changes after an
// operation is created.
func (r *repo) SaveOperation(ctx context.Context, op domain.Operation) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE operations SET status = $2, failure_reason = $3, limit_scope = $4, over_limit_confirmed = $5,
			confirm_expires_at = $6, deadline_at = $7, attempts = $8, next_attempt_at = $9, updated_at = $10,
			executed_at = $11, refunded_at = $12, completed_at = $13
		 WHERE id = $1`,
		op.ID, int16(op.Status), nullableCode(int16(op.Failure)), nullableCode(int16(op.LimitScope)),
		op.OverLimitConfirmed, utcPtr(op.ConfirmExpiresAt), utc(op.DeadlineAt), op.Attempts,
		utc(op.NextAttemptAt), utc(op.UpdatedAt), utcPtr(op.ExecutedAt), utcPtr(op.RefundedAt), utcPtr(op.CompletedAt))
	if err != nil {
		return fmt.Errorf("save operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

func (r *repo) operations(ctx context.Context, doing, sql string, args ...any) ([]domain.Operation, error) {
	rows, err := r.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", doing, err)
	}
	defer rows.Close()

	var out []domain.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", doing, err)
		}
		out = append(out, op)
	}

	return out, rows.Err()
}

// ListAccountOperations implements application.Repository.
func (r *repo) ListAccountOperations(ctx context.Context, accountID string, before *application.OperationCursor, limit int) ([]domain.Operation, error) {
	var (
		at *time.Time
		id *string
	)
	if before != nil {
		t := utc(before.CreatedAt)
		at, id = &t, &before.ID
	}

	return r.operations(ctx, "list operations",
		`SELECT `+operationColumns+` FROM operations
		 WHERE (account_id = $1 OR counterparty_id = $1)
		   AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $4`,
		accountID, at, id, limit)
}

func (r *repo) sum(ctx context.Context, doing, sql string, args ...any) (int64, error) {
	var total int64
	if err := r.q.QueryRow(ctx, sql, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("%s: %w", doing, err)
	}

	return total, nil
}

// SpentSince implements application.Repository.
func (r *repo) SpentSince(ctx context.Context, accountID string, since time.Time) (int64, error) {
	return r.sum(ctx, "spent since",
		`SELECT COALESCE(SUM(amount), 0)::bigint FROM operations
		 WHERE account_id = $1 AND spending AND status IN (2, 3, 4) AND executed_at >= $2`,
		accountID, utc(since))
}

// WalletSpentSince implements application.Repository.
func (r *repo) WalletSpentSince(ctx context.Context, walletID string, since time.Time) (int64, error) {
	return r.sum(ctx, "wallet spent since",
		`SELECT COALESCE(SUM(o.amount), 0)::bigint FROM operations o
		 JOIN accounts a ON a.id = o.account_id
		 WHERE a.wallet_id = $1 AND o.spending AND o.status IN (2, 3, 4) AND o.executed_at >= $2`,
		walletID, utc(since))
}

// PendingOutgoing implements application.Repository.
func (r *repo) PendingOutgoing(ctx context.Context, accountID string) (int64, error) {
	return r.sum(ctx, "pending outgoing",
		`SELECT COALESCE(SUM(amount), 0)::bigint FROM operations
		 WHERE account_id = $1 AND status = 2 AND kind IN (2, 3, 5)`,
		accountID)
}

// InFlightCount implements application.Repository.
func (r *repo) InFlightCount(ctx context.Context, accountID string) (int, error) {
	n, err := r.sum(ctx, "in flight",
		`SELECT COUNT(*) FROM operations
		 WHERE status IN (1, 2, 3) AND (account_id = $1 OR counterparty_id = $1)`,
		accountID)

	return int(n), err
}

// ClaimDue implements application.Repository. One statement both picks the operations (skipping
// any that another instance has locked this instant) and pushes their next attempt on, so two
// instances never get the same one. The lease grows with each attempt, up to twenty times its
// base length.
func (r *repo) ClaimDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Operation, error) {
	return r.operations(ctx, "claim due operations",
		`WITH due AS (
			SELECT id FROM operations
			WHERE status IN (2, 3) AND next_attempt_at <= $1
			ORDER BY next_attempt_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE operations o
		SET attempts = o.attempts + 1,
		    next_attempt_at = $1::timestamptz
		        + ($2::float8 * LEAST(o.attempts + 1, 20)) * interval '1 microsecond'
		FROM due
		WHERE o.id = due.id
		RETURNING `+prefixed("o", operationColumns),
		utc(now), float64(lease.Microseconds()), limit)
}

// DueConfirmations implements application.Repository.
func (r *repo) DueConfirmations(ctx context.Context, now time.Time, limit int) ([]domain.Operation, error) {
	return r.operations(ctx, "due confirmations",
		`SELECT `+operationColumns+` FROM operations
		 WHERE status = 1 AND confirm_expires_at <= $1
		 ORDER BY confirm_expires_at, id
		 LIMIT $2`,
		utc(now), limit)
}
