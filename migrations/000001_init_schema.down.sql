-- 000001_init_schema.down.sql
BEGIN;

DROP TRIGGER IF EXISTS ledger_entries_balanced_trg ON ledger_entries;
DROP FUNCTION IF EXISTS assert_transaction_balanced();
DROP VIEW IF EXISTS transaction_totals;

DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS wallets;

COMMIT;
