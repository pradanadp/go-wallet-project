-- 000001_init_schema.up.sql
-- Wallet & Ledger Service - core schema.
--
-- Conventions:
--   * Money is stored as a signed BIGINT in minor units (e.g. cents), which
--     avoids floating point rounding errors.
--   * Double-entry bookkeeping: every money movement creates one `transactions`
--     row plus >= 2 `ledger_entries` rows where total debit = total credit.
--   * `credit` increases a wallet balance, `debit` decreases it.
--   * Money coming from or going to the outside world is represented by a system
--     wallet (the "world account"), seeded by migration 000002.

BEGIN;

-- ---------------------------------------------------------------------------
-- wallets
-- ---------------------------------------------------------------------------
CREATE TABLE wallets (
    id            UUID        NOT NULL DEFAULT gen_random_uuid(),
    owner_id      UUID        NOT NULL,
    currency      VARCHAR(3)  NOT NULL,
    status        VARCHAR(16) NOT NULL DEFAULT 'active',
    is_system     BOOLEAN     NOT NULL DEFAULT FALSE,
    -- cached/materialized balance; ledger_entries stays the source of truth and
    -- this column is updated inside the same database transaction that writes
    -- the entry.
    balance_minor BIGINT      NOT NULL DEFAULT 0,
    -- counter incremented on every mutation; useful for audits/debugging and for
    -- a future optimistic lock.
    version       BIGINT      NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT wallets_pkey PRIMARY KEY (id),
    CONSTRAINT wallets_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_status_valid CHECK (status IN ('active', 'frozen', 'closed')),
    -- a user wallet must never go negative; the system wallet (world account) may.
    CONSTRAINT wallets_balance_guard CHECK (is_system OR balance_minor >= 0),
    CONSTRAINT wallets_version_non_negative CHECK (version >= 0)
);

CREATE INDEX wallets_owner_id_idx ON wallets (owner_id);

-- ---------------------------------------------------------------------------
-- transactions (header / journal)
-- ---------------------------------------------------------------------------
CREATE TABLE transactions (
    id              UUID        NOT NULL DEFAULT gen_random_uuid(),
    type            VARCHAR(16) NOT NULL,
    status          VARCHAR(16) NOT NULL DEFAULT 'posted',
    reference       VARCHAR(128) NOT NULL DEFAULT '',
    idempotency_key TEXT,
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT transactions_pkey PRIMARY KEY (id),
    CONSTRAINT transactions_type_valid   CHECK (type   IN ('topup', 'withdrawal', 'transfer')),
    CONSTRAINT transactions_status_valid CHECK (status IN ('posted', 'reversed'))
);

CREATE INDEX transactions_created_at_idx ON transactions (created_at DESC);
CREATE INDEX transactions_idempotency_key_idx
    ON transactions (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- ledger_entries (debit/credit rows)
-- ---------------------------------------------------------------------------
CREATE TABLE ledger_entries (
    -- BIGINT identity: monotonic, used as the pagination cursor (cheaper than
    -- OFFSET and free of drift).
    id             BIGINT      GENERATED ALWAYS AS IDENTITY NOT NULL,
    transaction_id UUID        NOT NULL,
    wallet_id      UUID        NOT NULL,
    direction      VARCHAR(8)  NOT NULL,
    amount_minor   BIGINT      NOT NULL,
    -- snapshot of the wallet balance after this entry was posted (audit trail).
    balance_after  BIGINT      NOT NULL,
    currency       VARCHAR(3)  NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_entries_pkey PRIMARY KEY (id),
    CONSTRAINT ledger_entries_direction_valid CHECK (direction IN ('debit', 'credit')),
    CONSTRAINT ledger_entries_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT ledger_entries_transaction_fk
        FOREIGN KEY (transaction_id) REFERENCES transactions (id) ON DELETE RESTRICT,
    CONSTRAINT ledger_entries_wallet_fk
        FOREIGN KEY (wallet_id) REFERENCES wallets (id) ON DELETE RESTRICT
);

-- main query: wallet history, newest first, cursor by id.
CREATE INDEX ledger_entries_wallet_id_id_idx ON ledger_entries (wallet_id, id DESC);
CREATE INDEX ledger_entries_transaction_id_idx ON ledger_entries (transaction_id);

-- ---------------------------------------------------------------------------
-- idempotency_keys
-- ---------------------------------------------------------------------------
-- The unique primary key on `key` is the anti double-spend mechanism for client
-- retries. Inserting this row takes a row lock for the duration of the database
-- transaction, so a retry with the same key waits and then detects the duplicate.
CREATE TABLE idempotency_keys (
    key           TEXT        NOT NULL,
    endpoint      VARCHAR(64) NOT NULL,
    -- sha256 of the canonical request body: same key + different body => 409.
    request_hash  TEXT        NOT NULL,
    status        VARCHAR(16) NOT NULL DEFAULT 'in_progress',
    status_code   INTEGER,
    response_body JSONB,
    resource_id   UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,

    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (key),
    CONSTRAINT idempotency_keys_status_valid CHECK (status IN ('in_progress', 'completed'))
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- ---------------------------------------------------------------------------
-- Audit helpers: make sure every transaction is balanced (debit = credit).
-- This view is also used for manual reconciliation:
--   SELECT * FROM transaction_totals WHERE total_debit <> total_credit;
-- ---------------------------------------------------------------------------
CREATE VIEW transaction_totals AS
SELECT transaction_id,
       COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'debit'), 0)  AS total_debit,
       COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'credit'), 0) AS total_credit,
       COUNT(*)                                                          AS entry_count
FROM ledger_entries
GROUP BY transaction_id;

-- CONSTRAINT TRIGGER, DEFERRABLE INITIALLY DEFERRED: validation runs at COMMIT
-- (after every entry row is written) instead of per statement.
-- Note: ledger_entries is append-only (never UPDATEd), so NEW/OLD.transaction_id
-- are always identical; COALESCE only handles INSERT (OLD is NULL) and DELETE
-- (NEW is NULL).
CREATE FUNCTION assert_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_tx_id  UUID;
    v_debit  BIGINT;
    v_credit BIGINT;
BEGIN
    v_tx_id := COALESCE(NEW.transaction_id, OLD.transaction_id);

    SELECT COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'debit'), 0),
           COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'credit'), 0)
      INTO v_debit, v_credit
      FROM ledger_entries
     WHERE transaction_id = v_tx_id;

    IF v_debit <> v_credit THEN
        RAISE EXCEPTION 'unbalanced transaction %: debit=% credit=%', v_tx_id, v_debit, v_credit
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced_trg
    AFTER INSERT OR UPDATE OR DELETE ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION assert_transaction_balanced();

COMMIT;
