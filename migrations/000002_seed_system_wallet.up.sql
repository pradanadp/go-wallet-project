-- 000002_seed_system_wallet.up.sql
-- The system wallet ("world account") represents money entering or leaving the
-- outside world (top-up & withdrawal). With this account every transaction stays
-- balanced in double-entry terms:
--   top-up     : DEBIT system wallet, CREDIT user wallet
--   withdrawal : DEBIT user wallet,   CREDIT system wallet
--   transfer   : DEBIT source wallet, CREDIT destination wallet
-- The system wallet balance therefore goes negative (it mirrors the total amount
-- of money circulating with users).
--
-- The ID is deterministic so it can be referenced from configuration
-- (SYSTEM_WALLET_ID).

BEGIN;

INSERT INTO wallets (id, owner_id, currency, status, is_system, balance_minor)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000000',
    'IDR',
    'active',
    TRUE,
    0
)
ON CONFLICT (id) DO NOTHING;

COMMIT;
