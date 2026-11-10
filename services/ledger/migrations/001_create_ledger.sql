-- +goose Up

-- The ledger is append-only. Rows are inserted and never changed or removed; triggers below make
-- the database refuse UPDATE, DELETE and TRUNCATE even from the table owner.
--
-- Every amount is stored as whole cents (the posted amount x 100) in a bigint, so every sum is
-- exact integer arithmetic. Conversion to and from decimal amounts happens only at the API edge.

CREATE TABLE ledger_entries (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     uuid        NOT NULL,
    -- 1, 2, 3, ... per account: the ledger's true order. A unique key, so a fork is impossible.
    seq            bigint      NOT NULL CHECK (seq > 0),
    transaction_id uuid        NOT NULL,
    direction      text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount         bigint      NOT NULL CHECK (amount > 0),
    -- clock_timestamp(), not now(): it is read at insert time, while the account lock is held, so
    -- created_at never runs backwards relative to seq (now() is the transaction's start time).
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),

    CONSTRAINT ledger_entries_account_seq_key UNIQUE (account_id, seq),
    -- One leg per transaction, account and direction: makes a retried post idempotent.
    CONSTRAINT ledger_entries_leg_key UNIQUE (account_id, transaction_id, direction)
);

CREATE INDEX ledger_entries_account_created_idx ON ledger_entries (account_id, created_at);

-- A checkpoint is an account's closing balance for a finished period (month, quarter, year or
-- five years). The current balance is the latest checkpoint plus the entries after last_seq, so
-- a balance never needs to sum an account's whole history.
CREATE TABLE ledger_checkpoints (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   uuid        NOT NULL,
    period       text        NOT NULL CHECK (period IN ('month', 'quarter', 'year', 'five_year')),
    period_start timestamptz NOT NULL,
    period_end   timestamptz NOT NULL,   -- exclusive: the instant the next period starts
    -- The balance after the entry with this seq.
    last_seq     bigint      NOT NULL CHECK (last_seq > 0),
    -- A negative closing balance means an overdraft got through; refuse to record it.
    balance      bigint      NOT NULL CHECK (balance >= 0),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),

    CHECK (period_end > period_start),
    CONSTRAINT ledger_checkpoints_period_key UNIQUE (account_id, period, period_start)
);

-- Latest checkpoint for an account, and latest of one period for the checkpointer.
CREATE INDEX ledger_checkpoints_latest_idx ON ledger_checkpoints (account_id, last_seq DESC);
CREATE INDEX ledger_checkpoints_chain_idx ON ledger_checkpoints (account_id, period, last_seq DESC);

-- +goose StatementBegin
CREATE FUNCTION ledger_reject_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: the ledger is append-only', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_change();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_change();

CREATE TRIGGER ledger_checkpoints_append_only
    BEFORE UPDATE OR DELETE ON ledger_checkpoints
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_change();

CREATE TRIGGER ledger_checkpoints_no_truncate
    BEFORE TRUNCATE ON ledger_checkpoints
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_change();

-- +goose Down

-- The ledger is append-only: its tables are never dropped by a migration.
SELECT 1;
