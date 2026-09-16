package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/example/wallet-service/internal/domain"
)

type ledgerRepository struct{ q querier }

func (r *ledgerRepository) CreateTransaction(ctx context.Context, t *domain.Transaction) error {
	// NULLIF stores an empty idempotency_key as NULL rather than an empty string,
	// keeping the partial index and the "no key" semantics correct.
	const query = `
INSERT INTO transactions (id, type, status, reference, idempotency_key, metadata, created_at)
VALUES ($1, $2, $3, $4, NULLIF($5::text, ''), $6, $7)`

	_, err := r.q.Exec(ctx, query,
		t.ID.String(),
		string(t.Type),
		string(t.Status),
		t.Reference,
		t.IdempotencyKey,
		t.Metadata,
		t.CreatedAt,
	)
	return mapError(err)
}

// InsertEntries writes the debit/credit rows.
//
// It deliberately uses an Exec loop (instead of COPY or a multi-row INSERT) because
// the number of entries per transaction is tiny (always 2) and a loop is easier to
// read and reports errors per row. The database constraint trigger validates that
// debit = credit at COMMIT.
func (r *ledgerRepository) InsertEntries(ctx context.Context, entries []domain.Entry) error {
	const query = `
INSERT INTO ledger_entries (transaction_id, wallet_id, direction, amount_minor, balance_after, currency, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

	for i, entry := range entries {
		_, err := r.q.Exec(ctx, query,
			entry.TransactionID.String(),
			entry.WalletID.String(),
			string(entry.Direction),
			entry.Amount.Int64(),
			entry.BalanceAfter.Int64(),
			entry.Currency.String(),
			entry.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("inserting ledger entry #%d: %w", i, mapError(err))
		}
	}
	return nil
}

// listRecordsQuery fetches one wallet's ledger history, newest first.
//
// The cursor uses `id < $2` (keyset) instead of OFFSET, so:
//   - the cost is constant because the (wallet_id, id DESC) index points straight at the position;
//   - no row is skipped or duplicated when new transactions arrive between two
//     requests - important for an append-only ledger.
//
// The counterparty is fetched with a LATERAL join to the other entry of the same
// transaction (no denormalized column, because the debit/credit pair is already the
// source of truth). COALESCE(... ::text, ”) keeps the result type non-NULL.
const listRecordsQuery = `
SELECT e.id,
       e.transaction_id,
       t.type,
       t.reference,
       e.direction,
       e.amount_minor,
       e.balance_after,
       e.currency,
       COALESCE(cp.wallet_id::text, '') AS counterparty_wallet_id,
       e.created_at
  FROM ledger_entries e
  JOIN transactions t ON t.id = e.transaction_id
  LEFT JOIN LATERAL (
      SELECT o.wallet_id
        FROM ledger_entries o
       WHERE o.transaction_id = e.transaction_id
         AND o.wallet_id <> e.wallet_id
       ORDER BY o.id
       LIMIT 1
  ) cp ON TRUE
 WHERE e.wallet_id = $1
   AND ($2::bigint = 0 OR e.id < $2::bigint)
 ORDER BY e.id DESC
 LIMIT $3`

func (r *ledgerRepository) ListRecordsByWallet(ctx context.Context, walletID domain.ID, afterEntryID int64, limit int) ([]domain.LedgerRecord, error) {
	rows, err := r.q.Query(ctx, listRecordsQuery, walletID.String(), afterEntryID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	records := make([]domain.LedgerRecord, 0, limit)
	for rows.Next() {
		var (
			entryID      int64
			transaction  string
			txnType      string
			reference    string
			direction    string
			amount       int64
			balanceAfter int64
			currency     string
			counterparty string
			createdAt    time.Time
		)
		if err := rows.Scan(&entryID, &transaction, &txnType, &reference, &direction,
			&amount, &balanceAfter, &currency, &counterparty, &createdAt); err != nil {
			return nil, mapError(err)
		}

		record := domain.LedgerRecord{
			EntryID:       entryID,
			TransactionID: domain.ID(transaction),
			Type:          domain.TransactionType(txnType),
			Reference:     reference,
			Direction:     domain.Direction(direction),
			Amount:        domain.Amount(amount),
			BalanceAfter:  domain.Amount(balanceAfter),
			Currency:      domain.Currency(currency),
			CreatedAt:     createdAt,
		}
		if counterparty != "" {
			id := domain.ID(counterparty)
			record.CounterpartyWalletID = &id
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return records, nil
}
