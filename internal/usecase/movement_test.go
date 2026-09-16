package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/example/wallet-service/internal/domain"
	"github.com/example/wallet-service/internal/usecase"
)

func TestTopUpCreditsUserAndDebitsWorldWallet(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)

	view, err := env.movement.TopUp(ctx, usecase.TopUpInput{
		WalletID:       walletID,
		AmountMinor:    50_000,
		Reference:      "topup-bca-001",
		IdempotencyKey: "topup-key-0001",
	})
	if err != nil {
		t.Fatalf("TopUp: %v", err)
	}

	if view.Type != "topup" || view.Status != "posted" {
		t.Errorf("view = %+v", view)
	}
	if view.BalanceAfter != 50_000 {
		t.Errorf("balance_after = %d, want 50000", view.BalanceAfter)
	}
	if view.Reference != "topup-bca-001" {
		t.Errorf("reference = %q", view.Reference)
	}
	if got := env.balanceOf(walletID); got != 50_000 {
		t.Errorf("wallet balance = %d, want 50000", got)
	}
	if got := env.balanceOf(systemWalletID); got != -50_000 {
		t.Errorf("world balance = %d, want -50000 (the double-entry mirror)", got)
	}
	if n := env.ledgerEntryCount(walletID); n != 1 {
		t.Errorf("wallet ledger entries = %d, want 1", n)
	}
	if len(env.store.entries) != 2 {
		t.Errorf("total ledger entries = %d, want 2 (debit + credit)", len(env.store.entries))
	}
}

func TestTopUpWithSameIdempotencyKeyDoesNotDoubleCredit(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)
	input := usecase.TopUpInput{
		WalletID:       walletID,
		AmountMinor:    25_000,
		Reference:      "topup-retry",
		IdempotencyKey: "topup-key-retry",
	}

	first, err := env.movement.TopUp(ctx, input)
	if err != nil {
		t.Fatalf("first TopUp: %v", err)
	}
	second, err := env.movement.TopUp(ctx, input)
	if err != nil {
		t.Fatalf("second TopUp (retry): %v", err)
	}

	if first.TransactionID != second.TransactionID {
		t.Errorf("different transaction: %s != %s", first.TransactionID, second.TransactionID)
	}
	if !second.Replayed {
		t.Error("the retry was not marked as Replayed")
	}
	if got := env.balanceOf(walletID); got != 25_000 {
		t.Errorf("balance = %d, want 25000 (no double top-up)", got)
	}
	if len(env.store.txns) != 1 {
		t.Errorf("transaction count = %d, want 1", len(env.store.txns))
	}
	if len(env.store.entries) != 2 {
		t.Errorf("ledger entry count = %d, want 2", len(env.store.entries))
	}
}

func TestTopUpWithReusedKeyButDifferentPayloadConflicts(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)

	if _, err := env.movement.TopUp(ctx, usecase.TopUpInput{
		WalletID:       walletID,
		AmountMinor:    10_000,
		IdempotencyKey: "topup-key-conflict",
	}); err != nil {
		t.Fatalf("first TopUp: %v", err)
	}

	_, err := env.movement.TopUp(ctx, usecase.TopUpInput{
		WalletID:       walletID,
		AmountMinor:    99_000,
		IdempotencyKey: "topup-key-conflict",
	})

	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
	if got := env.balanceOf(walletID); got != 10_000 {
		t.Errorf("balance = %d, want it unchanged at 10000", got)
	}
}

func TestTopUpRequiresIdempotencyKey(t *testing.T) {
	env := newTestEnv()
	walletID := env.createWallet(ownerA)

	_, err := env.movement.TopUp(context.Background(), usecase.TopUpInput{
		WalletID:    walletID,
		AmountMinor: 1_000,
	})

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}

func TestWithdrawRejectsInsufficientFundsAndKeepsLedgerClean(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)
	env.topUp(walletID, 5_000, "topup-before-withdraw")

	_, err := env.movement.Withdraw(ctx, usecase.WithdrawInput{
		WalletID:       walletID,
		AmountMinor:    9_000,
		IdempotencyKey: "withdraw-key-fail",
	})

	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}
	if got := env.balanceOf(walletID); got != 5_000 {
		t.Errorf("balance = %d, want it unchanged at 5000", got)
	}
	if len(env.store.txns) != 1 {
		t.Errorf("transaction count = %d, want 1 (a failed withdraw is not recorded)", len(env.store.txns))
	}
	if _, ok := env.store.idempotency["withdraw-key-fail"]; ok {
		t.Error("the idempotency claim was not rolled back when the transaction failed")
	}
}

func TestWithdrawSucceedsAfterRetryWhenKeyWasReleasedByFailure(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)

	// the withdraw fails: there is no balance yet.
	_, err := env.movement.Withdraw(ctx, usecase.WithdrawInput{
		WalletID: walletID, AmountMinor: 3_000, IdempotencyKey: "withdraw-key-01",
	})
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}

	// the balance is topped up, then the client retries with the same key.
	env.topUp(walletID, 10_000, "topup-funding")
	view, err := env.movement.Withdraw(ctx, usecase.WithdrawInput{
		WalletID: walletID, AmountMinor: 3_000, IdempotencyKey: "withdraw-key-01",
	})
	if err != nil {
		t.Fatalf("Withdraw retry: %v", err)
	}
	if view.Replayed {
		t.Error("a retry after a business failure must execute fresh, not replay")
	}
	if got := env.balanceOf(walletID); got != 7_000 {
		t.Errorf("balance = %d, want 7000", got)
	}
}

func TestTransferMovesFundsAtomicallyBetweenWallets(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	from := env.createWallet(ownerA)
	to := env.createWallet(ownerB)
	env.topUp(from, 100_000, "topup-sender")

	view, err := env.movement.Transfer(ctx, usecase.TransferInput{
		FromWalletID:   from,
		ToWalletID:     to,
		AmountMinor:    40_000,
		Reference:      "transfer-invoice-42",
		IdempotencyKey: "transfer-key-0001",
	})
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	if view.WalletID != from {
		t.Errorf("wallet_id = %s, want the source wallet %s", view.WalletID, from)
	}
	if view.CounterpartyWalletID != to {
		t.Errorf("counterparty = %s, ingin %s", view.CounterpartyWalletID, to)
	}
	if view.BalanceAfter != 60_000 {
		t.Errorf("sender balance_after = %d, want 60000", view.BalanceAfter)
	}
	if got := env.balanceOf(from); got != 60_000 {
		t.Errorf("sender balance = %d, want 60000", got)
	}
	if got := env.balanceOf(to); got != 40_000 {
		t.Errorf("receiver balance = %d, want 40000", got)
	}

	// double-entry: 1 debit for the sender + 1 credit for the receiver; world untouched.
	var debits, credits int64
	for _, e := range env.store.entries {
		if e.TransactionID.String() != view.TransactionID {
			continue
		}
		if e.Direction == domain.DirectionDebit {
			debits += e.Amount.Int64()
		} else {
			credits += e.Amount.Int64()
		}
		if e.WalletID.String() == systemWalletID {
			t.Error("a transfer must not touch the system wallet")
		}
	}
	if debits != credits || debits != 40_000 {
		t.Errorf("debit=%d credit=%d, want both to be 40000", debits, credits)
	}
}

func TestTransferInsufficientFundsLeavesBothWalletsUntouched(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	from := env.createWallet(ownerA)
	to := env.createWallet(ownerB)
	env.topUp(from, 10_000, "topup-sender")

	_, err := env.movement.Transfer(ctx, usecase.TransferInput{
		FromWalletID:   from,
		ToWalletID:     to,
		AmountMinor:    50_000,
		IdempotencyKey: "transfer-key-fail",
	})

	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}
	if got := env.balanceOf(from); got != 10_000 {
		t.Errorf("sender balance = %d, want it unchanged at 10000", got)
	}
	if got := env.balanceOf(to); got != 0 {
		t.Errorf("receiver balance = %d, want it unchanged at 0", got)
	}
}

func TestTransferToUnknownWalletRollsBackSourceDebit(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	from := env.createWallet(ownerA)
	env.topUp(from, 10_000, "topup-sender")

	_, err := env.movement.Transfer(ctx, usecase.TransferInput{
		FromWalletID:   from,
		ToWalletID:     "99999999-9999-4999-8999-999999999999",
		AmountMinor:    1_000,
		IdempotencyKey: "transfer-key-unknown",
	})

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if got := env.balanceOf(from); got != 10_000 {
		t.Errorf("sender balance = %d, want it unchanged at 10000 (rollback)", got)
	}
	if len(env.store.entries) != 2 {
		t.Errorf("ledger entry count = %d, want 2 (a failed transfer writes no entries)", len(env.store.entries))
	}
}

func TestTransferRejectsSelfTransfer(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)

	_, err := env.movement.Transfer(ctx, usecase.TransferInput{
		FromWalletID:   walletID,
		ToWalletID:     walletID,
		AmountMinor:    1_000,
		IdempotencyKey: "transfer-key-self",
	})

	if !errors.Is(err, domain.ErrSelfTransfer) {
		t.Fatalf("error = %v, want ErrSelfTransfer", err)
	}
}

func TestConcurrentTopUpsWithSameKeyCreditOnce(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)

	const goroutines = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		txnIDs    = map[string]int{}
		replays   int
		successes int
	)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, err := env.movement.TopUp(ctx, usecase.TopUpInput{
				WalletID:       walletID,
				AmountMinor:    25_000,
				IdempotencyKey: "topup-key-concurrent",
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("TopUp: %v", err)
				return
			}
			successes++
			txnIDs[view.TransactionID]++
			if view.Replayed {
				replays++
			}
		}()
	}
	wg.Wait()

	if successes != goroutines {
		t.Fatalf("successes = %d, want %d", successes, goroutines)
	}
	if len(txnIDs) != 1 {
		t.Errorf("unique transaction count = %d, want 1", len(txnIDs))
	}
	if replays != goroutines-1 {
		t.Errorf("replays = %d, want %d", replays, goroutines-1)
	}
	if got := env.balanceOf(walletID); got != 25_000 {
		t.Errorf("balance = %d, want 25000", got)
	}
}

func TestTopUpOnSystemWalletIsForbidden(t *testing.T) {
	env := newTestEnv()

	_, err := env.movement.TopUp(context.Background(), usecase.TopUpInput{
		WalletID:       systemWalletID,
		AmountMinor:    1_000,
		IdempotencyKey: "topup-key-system",
	})

	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}
