//go:build integration

// These integration tests run the service against a real PostgreSQL.
//
// Unit tests already cover the business logic with fakes, but the two main
// guarantees of this system can only be proven on a real database:
//  1. idempotency is safe when twin requests arrive AT THE SAME TIME (competing on
//     the unique index, not merely in sequence);
//  2. bidirectional transfers do not deadlock (locks are taken in canonical order)
//     while balances stay consistent.
//
// Run:
//
//	TEST_DATABASE_URL='postgres://wallet:wallet@localhost:5432/wallet_test?sslmode=disable' \
//	  go test -tags=integration -race ./internal/repository/postgres/...
package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/wallet-service/internal/domain"
	"github.com/example/wallet-service/internal/platform/migrate"
	"github.com/example/wallet-service/internal/repository/postgres"
	"github.com/example/wallet-service/internal/usecase"
	"github.com/example/wallet-service/pkg/uuid"
)

const (
	defaultTestDSN = "postgres://wallet:wallet@localhost:5432/wallet_test?sslmode=disable"
	systemWalletID = "00000000-0000-0000-0000-000000000001"
	ownerA         = "11111111-1111-4111-8111-111111111111"
	ownerB         = "22222222-2222-4222-8222-222222222222"
)

const truncateSQL = `
TRUNCATE ledger_entries, transactions, idempotency_keys, wallets RESTART IDENTITY CASCADE;
INSERT INTO wallets (id, owner_id, currency, status, is_system, balance_minor, created_at, updated_at)
VALUES ('00000000-0000-0000-0000-000000000001',
        '00000000-0000-0000-0000-000000000000',
        'IDR', 'active', TRUE, 0, now(), now());`

var (
	setupOnce sync.Once
	setupErr  error
	testPool  *pgxpool.Pool
)

type testEnv struct {
	t        *testing.T
	pool     *pgxpool.Pool
	wallets  *usecase.WalletUseCase
	movement *usecase.MovementUseCase
}

// newTestEnv prepares a shared pool (migrations run once) and then empties the
// data so every test starts from a clean state.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	setupOnce.Do(func() {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			dsn = defaultTestDSN
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))

		if err := migrate.Run(context.Background(), migrate.CommandUp, dsn, log); err != nil {
			setupErr = fmt.Errorf("migrations: %w", err)
			return
		}

		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			setupErr = fmt.Errorf("parse dsn: %w", err)
			return
		}
		cfg.MaxConns = 20

		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			setupErr = fmt.Errorf("creating the pool: %w", err)
			return
		}
		testPool = pool
	})
	if setupErr != nil {
		t.Fatalf("test database setup failed: %v", setupErr)
	}
	if testPool == nil {
		t.Fatal("pool test nil")
	}

	ctx := context.Background()
	if _, err := testPool.Exec(ctx, truncateSQL); err != nil {
		t.Fatalf("reset database: %v", err)
	}

	uow := postgres.NewUnitOfWork(testPool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &testEnv{
		t:        t,
		pool:     testPool,
		wallets:  usecase.NewWalletUseCase(uow, time.Now, uuid.V4, time.Hour),
		movement: usecase.NewMovementUseCase(uow, time.Now, uuid.V4, domain.MustParseID(systemWalletID), time.Hour),
	}
}

func (e *testEnv) createWallet(owner string) string {
	e.t.Helper()
	view, err := e.wallets.CreateWallet(context.Background(), usecase.CreateWalletInput{OwnerID: owner, Currency: "IDR"})
	if err != nil {
		e.t.Fatalf("CreateWallet: %v", err)
	}
	return view.ID
}

func (e *testEnv) balance(walletID string) int64 {
	e.t.Helper()
	view, err := e.wallets.GetWallet(context.Background(), usecase.GetWalletInput{WalletID: walletID})
	if err != nil {
		e.t.Fatalf("GetWallet(%s): %v", walletID, err)
	}
	return view.BalanceMinor
}

func (e *testEnv) topUp(walletID string, amount int64, key string) *usecase.MovementView {
	e.t.Helper()
	view, err := e.movement.TopUp(context.Background(), usecase.TopUpInput{
		WalletID: walletID, AmountMinor: amount, IdempotencyKey: key,
	})
	if err != nil {
		e.t.Fatalf("TopUp: %v", err)
	}
	return view
}

// assertLedgerBalanced verifies the double-entry invariant directly in the database.
func (e *testEnv) assertLedgerBalanced() {
	e.t.Helper()

	var unbalanced int
	err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM transaction_totals WHERE total_debit <> total_credit`).Scan(&unbalanced)
	if err != nil {
		e.t.Fatalf("reconciliation query: %v", err)
	}
	if unbalanced != 0 {
		e.t.Fatalf("%d unbalanced transactions found", unbalanced)
	}

	var total int64
	if err := e.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(balance_minor), 0) FROM wallets`).Scan(&total); err != nil {
		e.t.Fatalf("total balance query: %v", err)
	}
	if total != 0 {
		// Because every money movement is recorded on two sides (including the world
		// account), the total balance of all wallets must always be zero.
		e.t.Fatalf("total balance of all wallets = %d, expected 0", total)
	}
}

func (e *testEnv) entryCount(walletID string) int {
	e.t.Helper()
	var count int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_entries WHERE wallet_id = $1`, walletID).Scan(&count); err != nil {
		e.t.Fatalf("entry count query: %v", err)
	}
	return count
}

func TestIntegrationTopUpWritesDoubleEntryLedger(t *testing.T) {
	env := newTestEnv(t)
	walletID := env.createWallet(ownerA)

	view := env.topUp(walletID, 50_000, "it-topup-0001")

	if view.BalanceAfter != 50_000 {
		t.Fatalf("balance_after = %d, want 50000", view.BalanceAfter)
	}
	if got := env.balance(walletID); got != 50_000 {
		t.Errorf("wallet balance = %d, want 50000", got)
	}
	if got := env.balance(systemWalletID); got != -50_000 {
		t.Errorf("world account balance = %d, want -50000", got)
	}
	if got := env.entryCount(walletID); got != 1 {
		t.Errorf("wallet entry count = %d, want 1", got)
	}

	var totalEntries int
	_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM ledger_entries`).Scan(&totalEntries)
	if totalEntries != 2 {
		t.Errorf("total entries = %d, want 2 (debit + credit)", totalEntries)
	}

	env.assertLedgerBalanced()
}

func TestIntegrationTransferMovesFundsAtomically(t *testing.T) {
	env := newTestEnv(t)
	from := env.createWallet(ownerA)
	to := env.createWallet(ownerB)
	env.topUp(from, 100_000, "it-topup-0002")

	view, err := env.movement.Transfer(context.Background(), usecase.TransferInput{
		FromWalletID: from, ToWalletID: to, AmountMinor: 40_000,
		Reference: "invoice-1", IdempotencyKey: "it-transfer-0001",
	})
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	if view.BalanceAfter != 60_000 || view.CounterpartyWalletID != to {
		t.Errorf("view = %+v", view)
	}
	if got := env.balance(from); got != 60_000 {
		t.Errorf("sender balance = %d, want 60000", got)
	}
	if got := env.balance(to); got != 40_000 {
		t.Errorf("receiver balance = %d, want 40000", got)
	}

	// The counterparty must be readable back from the history (exercises the LATERAL join).
	page, err := env.wallets.ListTransactions(context.Background(), usecase.ListTransactionsInput{WalletID: from})
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("record count = %d, want 2", len(page.Records))
	}
	if page.Records[0].Type != "transfer" || page.Records[0].Direction != "debit" {
		t.Errorf("newest record = %+v", page.Records[0])
	}
	if page.Records[0].CounterpartyWalletID != to {
		t.Errorf("counterparty = %q, want %q", page.Records[0].CounterpartyWalletID, to)
	}
	if page.Records[1].Type != "topup" || page.Records[1].CounterpartyWalletID != systemWalletID {
		t.Errorf("second record = %+v", page.Records[1])
	}

	env.assertLedgerBalanced()
}

func TestIntegrationInsufficientFundsLeavesNoTrace(t *testing.T) {
	env := newTestEnv(t)
	walletID := env.createWallet(ownerA)
	env.topUp(walletID, 5_000, "it-topup-0003")

	_, err := env.movement.Withdraw(context.Background(), usecase.WithdrawInput{
		WalletID: walletID, AmountMinor: 10_000, IdempotencyKey: "it-withdraw-0001",
	})
	if err == nil {
		t.Fatal("a withdraw above the balance should have failed")
	}
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}

	if got := env.balance(walletID); got != 5_000 {
		t.Errorf("balance = %d, want it unchanged at 5000", got)
	}

	var txnCount int
	_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM transactions`).Scan(&txnCount)
	if txnCount != 1 {
		t.Errorf("transaction count = %d, want 1 (a failed withdraw is not recorded)", txnCount)
	}

	// The idempotency claim must be rolled back too, so the key can be reused.
	var keyCount int
	_ = env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM idempotency_keys WHERE key = 'it-withdraw-0001'`).Scan(&keyCount)
	if keyCount != 0 {
		t.Errorf("the idempotency key was not rolled back (count=%d)", keyCount)
	}
}

// TestIntegrationConcurrentTopUpWithSameKey ensures twin requests arriving at the
// same time are still executed exactly once.
//
// This is the proof a unit test cannot give: here the serialization comes from the
// PostgreSQL unique index and row lock, not from an in-memory mutex.
func TestIntegrationConcurrentTopUpWithSameKey(t *testing.T) {
	env := newTestEnv(t)
	walletID := env.createWallet(ownerA)

	const goroutines = 10
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		txnIDs     = map[string]int{}
		replays    int
		errs       []error
		firstTimes int
	)

	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // tembak bersamaan
			view, err := env.movement.TopUp(context.Background(), usecase.TopUpInput{
				WalletID: walletID, AmountMinor: 25_000, IdempotencyKey: "it-concurrent-key",
			})

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			txnIDs[view.TransactionID]++
			if view.Replayed {
				replays++
			} else {
				firstTimes++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d errors occurred: %v", len(errs), errs[0])
	}
	if len(txnIDs) != 1 {
		t.Fatalf("unique transaction_id count = %d, want 1 (%v)", len(txnIDs), txnIDs)
	}
	if firstTimes != 1 {
		t.Errorf("first executions = %d, want exactly 1", firstTimes)
	}
	if replays != goroutines-1 {
		t.Errorf("replays = %d, want %d", replays, goroutines-1)
	}
	if got := env.balance(walletID); got != 25_000 {
		t.Fatalf("balance = %d, want 25000 (no double top-up)", got)
	}

	env.assertLedgerBalanced()
}

// TestIntegrationConcurrentBidirectionalTransfers proves that the canonical lock
// order prevents deadlock: A->B and B->A run concurrently in large numbers.
//
// Without `slices.Sort` before SELECT ... FOR UPDATE, PostgreSQL would detect a
// deadlock and abort one of the transactions (error 40P01).
func TestIntegrationConcurrentBidirectionalTransfers(t *testing.T) {
	env := newTestEnv(t)
	walletA := env.createWallet(ownerA)
	walletB := env.createWallet(ownerB)
	env.topUp(walletA, 500_000, "it-topup-a")
	env.topUp(walletB, 500_000, "it-topup-b")

	const rounds = 15
	const amount = 1_000

	var wg sync.WaitGroup
	errCh := make(chan error, rounds*2)

	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, err := env.movement.Transfer(context.Background(), usecase.TransferInput{
				FromWalletID: walletA, ToWalletID: walletB, AmountMinor: amount,
				IdempotencyKey: fmt.Sprintf("it-fwd-%d", i),
			})
			errCh <- err
		}(i)
		go func(i int) {
			defer wg.Done()
			_, err := env.movement.Transfer(context.Background(), usecase.TransferInput{
				FromWalletID: walletB, ToWalletID: walletA, AmountMinor: amount,
				IdempotencyKey: fmt.Sprintf("it-back-%d", i),
			})
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("transfer failed (possible deadlock): %v", err)
		}
	}

	// The same number of transfers goes each way, so final balances must match the initial ones.
	if got := env.balance(walletA); got != 500_000 {
		t.Errorf("balance A = %d, want 500000", got)
	}
	if got := env.balance(walletB); got != 500_000 {
		t.Errorf("balance B = %d, want 500000", got)
	}

	env.assertLedgerBalanced()
}

func TestIntegrationHistoryCursorPaginationHasNoGapsOrDuplicates(t *testing.T) {
	env := newTestEnv(t)
	walletID := env.createWallet(ownerA)

	const total = 7
	expected := make([]string, 0, total)
	for i := 0; i < total; i++ {
		view := env.topUp(walletID, int64(1_000*(i+1)), fmt.Sprintf("it-page-key-%d", i))
		expected = append(expected, view.TransactionID)
	}

	var (
		cursor    int64
		collected []usecase.LedgerRecordView
		pages     int
	)
	for {
		page, err := env.wallets.ListTransactions(context.Background(), usecase.ListTransactionsInput{
			WalletID: walletID, AfterEntryID: cursor, Limit: 3,
		})
		if err != nil {
			t.Fatalf("ListTransactions: %v", err)
		}
		pages++
		collected = append(collected, page.Records...)
		if !page.HasMore {
			break
		}
		cursor = page.NextEntryID
		if pages > total {
			t.Fatal("pagination did not stop")
		}
	}

	if len(collected) != total {
		t.Fatalf("row count = %d, want %d", len(collected), total)
	}
	seen := map[string]bool{}
	for i, record := range collected {
		wantTxn := expected[total-1-i] // newest first
		if record.TransactionID != wantTxn {
			t.Errorf("row %d = %s, want %s", i, record.TransactionID, wantTxn)
		}
		if seen[record.TransactionID] {
			t.Errorf("transaction %s appeared twice", record.TransactionID)
		}
		seen[record.TransactionID] = true
	}
}

func TestIntegrationIdempotencyConflictOnDifferentPayload(t *testing.T) {
	env := newTestEnv(t)
	walletID := env.createWallet(ownerA)
	env.topUp(walletID, 10_000, "it-conflict-key")

	_, err := env.movement.TopUp(context.Background(), usecase.TopUpInput{
		WalletID: walletID, AmountMinor: 90_000, IdempotencyKey: "it-conflict-key",
	})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
	if got := env.balance(walletID); got != 10_000 {
		t.Errorf("balance = %d, want it unchanged at 10000", got)
	}
	env.assertLedgerBalanced()
}

func TestIntegrationPurgeExpiredIdempotencyKeys(t *testing.T) {
	env := newTestEnv(t)
	walletID := env.createWallet(ownerA)
	env.topUp(walletID, 1_000, "it-purge-key")

	// Expired keys are simulated by moving expires_at into the past.
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE idempotency_keys SET expires_at = now() - interval '1 hour' WHERE key = 'it-purge-key'`); err != nil {
		t.Fatalf("moving expires_at into the past: %v", err)
	}

	maintenance := postgres.NewMaintenance(env.pool)
	deleted, err := maintenance.PurgeExpiredIdempotencyKeys(context.Background())
	if err != nil {
		t.Fatalf("PurgeExpiredIdempotencyKeys: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
}
