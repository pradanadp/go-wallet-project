-- 000002_seed_system_wallet.down.sql
-- Removes the system wallet. This fails when ledger_entries still reference it
-- (FK ON DELETE RESTRICT) - which is intentional: the ledger must never lose its
-- audit trail.
BEGIN;

DELETE FROM wallets
 WHERE id = '00000000-0000-0000-0000-000000000001'
   AND is_system = TRUE
   AND NOT EXISTS (SELECT 1 FROM ledger_entries e WHERE e.wallet_id = wallets.id);

COMMIT;
