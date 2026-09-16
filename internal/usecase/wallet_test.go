package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/example/wallet-service/internal/domain"
	"github.com/example/wallet-service/internal/usecase"
)

func TestCreateWallet(t *testing.T) {
	env := newTestEnv()

	view, err := env.wallets.CreateWallet(context.Background(), usecase.CreateWalletInput{
		OwnerID:  ownerA,
		Currency: "IDR",
	})
	if err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}

	if view.ID == "" {
		t.Error("wallet id is empty")
	}
	if view.OwnerID != ownerA || view.Currency != "IDR" || view.Status != "active" {
		t.Errorf("view = %+v", view)
	}
	if view.BalanceMinor != 0 {
		t.Errorf("initial balance = %d, want 0", view.BalanceMinor)
	}
	if len(env.store.wallets) != 2 { // system wallet + the new wallet
		t.Errorf("wallet count = %d, want 2", len(env.store.wallets))
	}
}

func TestCreateWalletValidation(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()

	cases := []struct {
		name  string
		input usecase.CreateWalletInput
	}{
		{name: "owner is not a uuid", input: usecase.CreateWalletInput{OwnerID: "abc", Currency: "IDR"}},
		{name: "invalid currency", input: usecase.CreateWalletInput{OwnerID: ownerA, Currency: "rp"}},
		{name: "empty currency", input: usecase.CreateWalletInput{OwnerID: ownerA}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.wallets.CreateWallet(ctx, tc.input)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestCreateWalletIsIdempotentWhenKeyRepeated(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	input := usecase.CreateWalletInput{OwnerID: ownerA, Currency: "IDR", IdempotencyKey: "create-wallet-0001"}

	first, err := env.wallets.CreateWallet(ctx, input)
	if err != nil {
		t.Fatalf("first CreateWallet: %v", err)
	}
	second, err := env.wallets.CreateWallet(ctx, input)
	if err != nil {
		t.Fatalf("second CreateWallet: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("a second wallet was created: %s != %s", first.ID, second.ID)
	}
	if !second.Replayed {
		t.Error("the second response was not marked as Replayed")
	}
	if first.Replayed {
		t.Error("the first response must not be marked as Replayed")
	}
	if len(env.store.wallets) != 2 {
		t.Errorf("wallet count = %d, want 2 (idempotency failed)", len(env.store.wallets))
	}
}

func TestGetWalletNotFound(t *testing.T) {
	env := newTestEnv()

	_, err := env.wallets.GetWallet(context.Background(), usecase.GetWalletInput{
		WalletID: "99999999-9999-4999-8999-999999999999",
	})

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestListTransactionsPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	walletID := env.createWallet(ownerA)

	seen := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		view := env.topUp(walletID, int64(1_000*(i+1)), "")
		seen = append(seen, view.TransactionID)
	}

	var (
		cursor    int64
		collected []usecase.LedgerRecordView
		pageCount int
		wantOrder = reverse(seen)
	)
	for {
		page, err := env.wallets.ListTransactions(ctx, usecase.ListTransactionsInput{
			WalletID:     walletID,
			AfterEntryID: cursor,
			Limit:        2,
		})
		if err != nil {
			t.Fatalf("ListTransactions: %v", err)
		}
		pageCount++
		collected = append(collected, page.Records...)
		if !page.HasMore {
			break
		}
		cursor = page.NextEntryID
		if pageCount > 5 {
			t.Fatal("pagination did not stop (the cursor probably stopped advancing)")
		}
	}

	if pageCount != 3 {
		t.Errorf("page count = %d, want 3", pageCount)
	}
	if len(collected) != 5 {
		t.Fatalf("row count = %d, want 5", len(collected))
	}
	for i, rec := range collected {
		if rec.TransactionID != wantOrder[i] {
			t.Errorf("row %d = %s, want %s (newest first)", i, rec.TransactionID, wantOrder[i])
		}
	}
	if collected[0].Type != "topup" || collected[0].Direction != "credit" {
		t.Errorf("first record = %+v", collected[0])
	}
	if collected[0].BalanceAfterMinor != 15_000 {
		t.Errorf("balance_after = %d, want 15000", collected[0].BalanceAfterMinor)
	}
	if collected[0].CounterpartyWalletID != systemWalletID {
		t.Errorf("counterparty = %q, ingin %q", collected[0].CounterpartyWalletID, systemWalletID)
	}
}

func TestListTransactionsRejectsInvalidLimit(t *testing.T) {
	env := newTestEnv()
	walletID := env.createWallet(ownerA)

	_, err := env.wallets.ListTransactions(context.Background(), usecase.ListTransactionsInput{
		WalletID: walletID,
		Limit:    usecase.MaxPageLimit + 1,
	})

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}

func TestListTransactionsForUnknownWalletReturnsNotFound(t *testing.T) {
	env := newTestEnv()

	_, err := env.wallets.ListTransactions(context.Background(), usecase.ListTransactionsInput{
		WalletID: "99999999-9999-4999-8999-999999999999",
	})

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func reverse(in []string) []string {
	out := make([]string, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}
