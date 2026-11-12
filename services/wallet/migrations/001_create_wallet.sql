-- +goose Up

-- Every id is a UUID, the user's included. Every enum is a smallint with an explicit code and a
-- CHECK; the Go enums in internal/domain carry the same codes, and a test keeps the two in step.
-- Amounts are whole cents in bigint. Times come from the application's clock, not now().

CREATE TABLE wallets (
    id          uuid        PRIMARY KEY,
    user_id     uuid        NOT NULL,
    daily_limit bigint      CHECK (daily_limit IS NULL OR daily_limit > 0),
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,

    CONSTRAINT wallets_user_key UNIQUE (user_id)
);

CREATE TABLE accounts (
    id          uuid        PRIMARY KEY,
    wallet_id   uuid        NOT NULL REFERENCES wallets (id),
    name        text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    status      smallint    NOT NULL CHECK (status IN (1, 2, 3)),            -- 1 active, 2 frozen, 3 closed
    daily_limit bigint      CHECK (daily_limit IS NULL OR daily_limit > 0),
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    closed_at   timestamptz,

    CONSTRAINT accounts_closed_at CHECK ((status = 3) = (closed_at IS NOT NULL))
);

CREATE INDEX accounts_wallet_idx ON accounts (wallet_id, created_at, id);

-- A hold reserves part of an account's money. captured_by is added below, once operations exist.
CREATE TABLE holds (
    id              uuid        PRIMARY KEY,
    account_id      uuid        NOT NULL REFERENCES accounts (id),
    amount          bigint      NOT NULL CHECK (amount > 0),
    reason          text        NOT NULL,
    expires_at      timestamptz NOT NULL,
    released_at     timestamptz,
    captured_by     uuid,
    initiator       text        NOT NULL,
    idempotency_key uuid        NOT NULL,
    request_hash    text        NOT NULL,
    created_at      timestamptz NOT NULL,

    CONSTRAINT holds_idempotency_key UNIQUE (initiator, idempotency_key),
    CONSTRAINT holds_released_or_captured CHECK (released_at IS NULL OR captured_by IS NULL)
);

CREATE INDEX holds_active_idx ON holds (account_id) WHERE released_at IS NULL AND captured_by IS NULL;

-- The journal of money movements. An operation's id is also the ledger transaction id of each of
-- its ledger calls, which is what makes every call safe to repeat.
CREATE TABLE operations (
    id                   uuid        PRIMARY KEY,
    kind                 smallint    NOT NULL CHECK (kind IN (1, 2, 3, 4, 5)),   -- 1 credit, 2 debit, 3 transfer, 4 deposit, 5 withdrawal
    account_id           uuid        NOT NULL REFERENCES accounts (id),
    counterparty_id      uuid        REFERENCES accounts (id),
    amount               bigint      NOT NULL CHECK (amount > 0),
    hold_id              uuid        REFERENCES holds (id),
    status               smallint    NOT NULL CHECK (status IN (1, 2, 3, 4, 5)), -- 1 awaiting confirmation, 2 pending, 3 settling, 4 completed, 5 failed
    failure_reason       smallint    CHECK (failure_reason IN (1, 2, 3, 4, 5)),  -- 1 insufficient funds, 2 timed out, 3 confirmation expired, 4 rejected, 5 cancelled
    limit_scope          smallint    CHECK (limit_scope IN (1, 2)),              -- 1 account, 2 wallet
    initiator            text        NOT NULL,
    idempotency_key      uuid        NOT NULL,
    request_hash         text        NOT NULL,
    spending             boolean     NOT NULL,
    over_limit_confirmed boolean     NOT NULL DEFAULT false,
    confirm_expires_at   timestamptz,
    deadline_at          timestamptz NOT NULL,
    attempts             integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at      timestamptz NOT NULL,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    executed_at          timestamptz,
    refunded_at          timestamptz,
    completed_at         timestamptz,

    CONSTRAINT operations_idempotency_key UNIQUE (initiator, idempotency_key),
    CONSTRAINT operations_transfer_has_counterparty CHECK ((kind = 3) = (counterparty_id IS NOT NULL)),
    CONSTRAINT operations_counterparty_differs CHECK (counterparty_id IS NULL OR counterparty_id <> account_id),
    CONSTRAINT operations_failure_reason CHECK ((status = 5) = (failure_reason IS NOT NULL)),
    CONSTRAINT operations_awaiting CHECK (status <> 1 OR (limit_scope IS NOT NULL AND confirm_expires_at IS NOT NULL)),
    CONSTRAINT operations_executed CHECK (status NOT IN (2, 3, 4) OR executed_at IS NOT NULL),
    CONSTRAINT operations_completed CHECK ((status IN (4, 5)) = (completed_at IS NOT NULL))
);

ALTER TABLE holds
    ADD CONSTRAINT holds_captured_by_fkey FOREIGN KEY (captured_by) REFERENCES operations (id);

CREATE INDEX operations_account_idx ON operations (account_id, created_at DESC, id DESC);
CREATE INDEX operations_counterparty_idx ON operations (counterparty_id, created_at DESC, id DESC) WHERE counterparty_id IS NOT NULL;
CREATE INDEX operations_due_idx ON operations (next_attempt_at, id) WHERE status IN (2, 3);
CREATE INDEX operations_confirmation_idx ON operations (confirm_expires_at, id) WHERE status = 1;
CREATE INDEX operations_spent_idx ON operations (account_id, executed_at) WHERE spending AND status IN (2, 3, 4);

-- +goose Down

DROP TABLE operations CASCADE;
DROP TABLE holds;
DROP TABLE accounts;
DROP TABLE wallets;
