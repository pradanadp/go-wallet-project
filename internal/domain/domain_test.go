package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/example/wallet-service/internal/domain"
)

var fixedTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const (
	walletIDA = "11111111-1111-4111-8111-111111111111"
	walletIDB = "22222222-2222-4222-8222-222222222222"
	worldID   = "00000000-0000-0000-0000-000000000001"
	ownerID   = "33333333-3333-4333-8333-333333333333"
)

func newUserWallet(id string, balance domain.Amount) *domain.Wallet {
	return &domain.Wallet{
		ID:        domain.MustParseID(id),
		OwnerID:   domain.MustParseID(ownerID),
		Currency:  domain.CurrencyIDR,
		Status:    domain.WalletStatusActive,
		Balance:   balance,
		CreatedAt: fixedTime,
		UpdatedAt: fixedTime,
	}
}

func newWorldWallet(balance domain.Amount) *domain.Wallet {
	return &domain.Wallet{
		ID:       domain.MustParseID(worldID),
		OwnerID:  domain.NilID,
		Currency: domain.CurrencyIDR,
		Status:   domain.WalletStatusActive,
		IsSystem: true,
		Balance:  balance,
	}
}

func TestWalletDebitRejectsInsufficientFunds(t *testing.T) {
	w := newUserWallet(walletIDA, 5_000)

	_, err := w.Debit(10_000)

	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v, ingin ErrInsufficientFunds", err)
	}
	var insufficient *domain.InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("error %v does not carry InsufficientFundsError details", err)
	}
	if insufficient.Balance != 5_000 || insufficient.Required != 10_000 {
		t.Errorf("balance detail = %d/%d, want 5000/10000", insufficient.Balance, insufficient.Required)
	}
	if w.Balance != 5_000 {
		t.Errorf("balance changed to %d after a failed debit", w.Balance)
	}
}

func TestWalletDebitAllowsSystemWalletToGoNegative(t *testing.T) {
	world := newWorldWallet(0)

	after, err := world.Debit(10_000)

	if err != nil {
		t.Fatalf("system wallet debit failed: %v", err)
	}
	if after != -10_000 || world.Balance != -10_000 {
		t.Errorf("world balance = %d, want -10000", world.Balance)
	}
}

func TestWalletDebitRejectsFrozenWallet(t *testing.T) {
	w := newUserWallet(walletIDA, 10_000)
	w.Status = domain.WalletStatusFrozen

	_, err := w.Debit(1_000)

	if !errors.Is(err, domain.ErrWalletNotActive) {
		t.Fatalf("error = %v, ingin ErrWalletNotActive", err)
	}
}

func TestPostTopUpDebitsWorldAndCreditsUser(t *testing.T) {
	world := newWorldWallet(0)
	user := newUserWallet(walletIDA, 1_000)

	postings, err := domain.PostTopUp(world, user, 5_000)
	if err != nil {
		t.Fatalf("PostTopUp: %v", err)
	}
	if err := domain.AssertBalanced(postings); err != nil {
		t.Fatalf("postings are not balanced: %v", err)
	}
	if user.Balance != 6_000 {
		t.Errorf("user balance = %d, want 6000", user.Balance)
	}
	if world.Balance != -5_000 {
		t.Errorf("world balance = %d, want -5000", world.Balance)
	}
	if postings[0].Direction != domain.DirectionDebit || postings[1].Direction != domain.DirectionCredit {
		t.Errorf("wrong posting order: %+v", postings)
	}
	if postings[1].BalanceAfter != 6_000 {
		t.Errorf("balance_after = %d, ingin 6000", postings[1].BalanceAfter)
	}
}

func TestPostWithdrawalDebitsUserFirst(t *testing.T) {
	world := newWorldWallet(0)
	user := newUserWallet(walletIDA, 10_000)

	if _, err := domain.PostWithdrawal(world, user, 4_000); err != nil {
		t.Fatalf("PostWithdrawal: %v", err)
	}
	if user.Balance != 6_000 || world.Balance != 4_000 {
		t.Errorf("user/world balance = %d/%d, want 6000/4000", user.Balance, world.Balance)
	}
}

func TestPostWithdrawalRejectsInsufficientFunds(t *testing.T) {
	world := newWorldWallet(0)
	user := newUserWallet(walletIDA, 100)

	_, err := domain.PostWithdrawal(world, user, 200)

	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v, ingin ErrInsufficientFunds", err)
	}
	if world.Balance != 0 {
		t.Errorf("world balance changed to %d even though the transaction failed", world.Balance)
	}
}

func TestPostTransferMovesFundsBetweenWallets(t *testing.T) {
	from := newUserWallet(walletIDA, 10_000)
	to := newUserWallet(walletIDB, 0)

	postings, err := domain.PostTransfer(from, to, 2_500)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := domain.AssertBalanced(postings); err != nil {
		t.Fatalf("postings are not balanced: %v", err)
	}
	if from.Balance != 7_500 || to.Balance != 2_500 {
		t.Errorf("from/to balance = %d/%d, want 7500/2500", from.Balance, to.Balance)
	}
}

func TestPostTransferRejectsSelfTransfer(t *testing.T) {
	from := newUserWallet(walletIDA, 10_000)
	to := newUserWallet(walletIDA, 0)

	_, err := domain.PostTransfer(from, to, 100)

	if !errors.Is(err, domain.ErrSelfTransfer) {
		t.Fatalf("error = %v, ingin ErrSelfTransfer", err)
	}
}

func TestPostTransferRejectsCurrencyMismatch(t *testing.T) {
	from := newUserWallet(walletIDA, 10_000)
	to := newUserWallet(walletIDB, 0)
	to.Currency = "USD"

	_, err := domain.PostTransfer(from, to, 100)

	if !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("error = %v, ingin ErrCurrencyMismatch", err)
	}
	if from.Balance != 10_000 {
		t.Errorf("from balance changed to %d even though the transfer was rejected", from.Balance)
	}
}

func TestPostTopUpRejectsSystemWalletAsUser(t *testing.T) {
	world := newWorldWallet(0)
	alsoWorld := newWorldWallet(0)

	_, err := domain.PostTopUp(world, alsoWorld, 100)

	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, ingin ErrForbidden", err)
	}
}

func TestAssertBalancedRejectsUnbalancedPostings(t *testing.T) {
	postings := []domain.Posting{
		{WalletID: domain.MustParseID(walletIDA), Direction: domain.DirectionDebit, Amount: 100, Currency: domain.CurrencyIDR},
		{WalletID: domain.MustParseID(walletIDB), Direction: domain.DirectionCredit, Amount: 90, Currency: domain.CurrencyIDR},
	}

	err := domain.AssertBalanced(postings)

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, ingin ErrValidation", err)
	}
}

func TestAssertBalancedRejectsSingleEntry(t *testing.T) {
	postings := []domain.Posting{
		{WalletID: domain.MustParseID(walletIDA), Direction: domain.DirectionDebit, Amount: 100, Currency: domain.CurrencyIDR},
	}

	if err := domain.AssertBalanced(postings); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, ingin ErrValidation", err)
	}
}

func TestNewAmountValidation(t *testing.T) {
	cases := []struct {
		name  string
		minor int64
		valid bool
	}{
		{name: "zero", minor: 0, valid: false},
		{name: "negative", minor: -1, valid: false},
		{name: "positive", minor: 1, valid: true},
		{name: "above the maximum", minor: int64(domain.MaxAmount) + 1, valid: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.NewAmount("amount_minor", tc.minor)
			if tc.valid && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.valid && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, ingin ErrValidation", err)
			}
		})
	}
}

func TestParseIDNormalizesAndRejects(t *testing.T) {
	id, err := domain.ParseID("wallet_id", "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}
	if id.String() != walletIDA {
		t.Errorf("id = %s, ingin %s", id, walletIDA)
	}

	upper, err := domain.ParseID("wallet_id", "11111111-1111-4111-8111-11111111111A")
	if err != nil {
		t.Fatalf("ParseID uppercase: %v", err)
	}
	if upper.String() != "11111111-1111-4111-8111-11111111111a" {
		t.Errorf("id = %s, ingin lowercase 11111111-1111-4111-8111-11111111111a", upper)
	}

	if _, err := domain.ParseID("wallet_id", "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, ingin ErrValidation", err)
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		valid bool
	}{
		{name: "empty", key: "", valid: false},
		{name: "too short", key: "abc", valid: false},
		{name: "contains a space", key: "abcdefg h", valid: false},
		{name: "valid", key: "topup-2026-0001", valid: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidateIdempotencyKey(tc.key)
			if tc.valid && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.valid && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, ingin ErrValidation", err)
			}
		})
	}
}
