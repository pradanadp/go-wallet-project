package domain

import (
	"fmt"
	"time"
)

// Direction is the booking side of a ledger row from the wallet's point of
// view: `credit` increases the balance, `debit` decreases it.
// The side is explicit (instead of just a +/- sign on the amount) so the ledger
// can be audited without guessing a sign convention.
type Direction string

const (
	DirectionDebit  Direction = "debit"
	DirectionCredit Direction = "credit"
)

// TransactionType is the journal type.
type TransactionType string

const (
	TransactionTypeTopUp      TransactionType = "topup"
	TransactionTypeWithdrawal TransactionType = "withdrawal"
	TransactionTypeTransfer   TransactionType = "transfer"
)

// TransactionStatus is the journal state. This service only has `posted`
// (transactions are always atomic; there is no pending state).
type TransactionStatus string

const TransactionStatusPosted TransactionStatus = "posted"

// Transaction is the journal header (one money movement).
type Transaction struct {
	ID             ID
	Type           TransactionType
	Status         TransactionStatus
	Reference      string
	IdempotencyKey string
	Metadata       map[string]any
	CreatedAt      time.Time
}

// NewTransaction creates a journal header with status posted.
func NewTransaction(t TransactionType, reference, idempotencyKey string, metadata map[string]any, now time.Time) *Transaction {
	if metadata == nil {
		metadata = map[string]any{}
	}
	return &Transaction{
		Type:           t,
		Status:         TransactionStatusPosted,
		Reference:      reference,
		IdempotencyKey: idempotencyKey,
		Metadata:       metadata,
		CreatedAt:      now.UTC(),
	}
}

// Entry is a persisted ledger row (the source of truth for balances).
// BalanceAfter is a snapshot of the wallet balance right after this entry was
// posted; it is stored on purpose for audit and reconciliation.
type Entry struct {
	ID            int64
	TransactionID ID
	WalletID      ID
	Direction     Direction
	Amount        Amount
	BalanceAfter  Amount
	Currency      Currency
	CreatedAt     time.Time
}

// Posting is a planned ledger row (no ID/transaction ID yet).
type Posting struct {
	WalletID     ID
	Direction    Direction
	Amount       Amount
	BalanceAfter Amount
	Currency     Currency
}

// Entry converts a Posting into an Entry ready to be inserted.
func (p Posting) Entry(transactionID ID, now time.Time) Entry {
	return Entry{
		TransactionID: transactionID,
		WalletID:      p.WalletID,
		Direction:     p.Direction,
		Amount:        p.Amount,
		BalanceAfter:  p.BalanceAfter,
		Currency:      p.Currency,
		CreatedAt:     now.UTC(),
	}
}

// AssertBalanced ensures total debit == total credit and that at least two rows
// exist. It is the application-side mirror of the `ledger_entries_balanced_trg`
// constraint trigger in PostgreSQL: double-entry bugs are caught before touching
// the database, with a much more informative error message.
func AssertBalanced(postings []Posting) error {
	if len(postings) < 2 {
		return fmt.Errorf("%w: a transaction needs at least 2 ledger entries, got %d", ErrValidation, len(postings))
	}
	var debit, credit Amount
	for _, p := range postings {
		switch p.Direction {
		case DirectionDebit:
			debit += p.Amount
		case DirectionCredit:
			credit += p.Amount
		default:
			return fmt.Errorf("%w: unknown direction %q", ErrValidation, p.Direction)
		}
	}
	if debit != credit {
		return fmt.Errorf("%w: unbalanced transaction (debit=%d, credit=%d)", ErrValidation, debit, credit)
	}
	return nil
}

// PostTopUp records money entering from outside the system:
//
//	DEBIT  world account (sumber dana eksternal)
//	CREDIT wallet user
//
// Both wallet balances are mutated here; the caller must roll back the database
// transaction when this function returns an error.
func PostTopUp(world, user *Wallet, amount Amount) ([]Posting, error) {
	if !world.IsSystem {
		return nil, &ForbiddenError{Reason: "the top-up counterparty must be the system wallet"}
	}
	if err := user.ensureOperable(); err != nil {
		return nil, err
	}
	if world.Currency != user.Currency {
		return nil, &CurrencyMismatchError{From: world.Currency, To: user.Currency}
	}

	// All validation finishes before mutation so there is no half-applied state.
	worldAfter, err := world.Debit(amount)
	if err != nil {
		return nil, err
	}
	userAfter, err := user.Credit(amount)
	if err != nil {
		return nil, err
	}

	return []Posting{
		{WalletID: world.ID, Direction: DirectionDebit, Amount: amount, BalanceAfter: worldAfter, Currency: world.Currency},
		{WalletID: user.ID, Direction: DirectionCredit, Amount: amount, BalanceAfter: userAfter, Currency: user.Currency},
	}, nil
}

// PostWithdrawal records money leaving the system:
//
//	DEBIT  wallet user
//	CREDIT world account
func PostWithdrawal(world, user *Wallet, amount Amount) ([]Posting, error) {
	if !world.IsSystem {
		return nil, &ForbiddenError{Reason: "the withdrawal counterparty must be the system wallet"}
	}
	if err := user.ensureOperable(); err != nil {
		return nil, err
	}
	if world.Currency != user.Currency {
		return nil, &CurrencyMismatchError{From: user.Currency, To: world.Currency}
	}

	userAfter, err := user.Debit(amount)
	if err != nil {
		return nil, err
	}
	worldAfter, err := world.Credit(amount)
	if err != nil {
		return nil, err
	}

	return []Posting{
		{WalletID: user.ID, Direction: DirectionDebit, Amount: amount, BalanceAfter: userAfter, Currency: user.Currency},
		{WalletID: world.ID, Direction: DirectionCredit, Amount: amount, BalanceAfter: worldAfter, Currency: world.Currency},
	}, nil
}

// PostTransfer records a balance move between two user wallets:
//
//	DEBIT  wallet asal
//	CREDIT wallet tujuan
func PostTransfer(from, to *Wallet, amount Amount) ([]Posting, error) {
	if from.ID == to.ID {
		return nil, &SelfTransferError{WalletID: from.ID}
	}
	if err := from.ensureOperable(); err != nil {
		return nil, err
	}
	if err := to.ensureOperable(); err != nil {
		return nil, err
	}
	if from.Currency != to.Currency {
		return nil, &CurrencyMismatchError{From: from.Currency, To: to.Currency}
	}

	fromAfter, err := from.Debit(amount)
	if err != nil {
		return nil, err
	}
	toAfter, err := to.Credit(amount)
	if err != nil {
		return nil, err
	}

	return []Posting{
		{WalletID: from.ID, Direction: DirectionDebit, Amount: amount, BalanceAfter: fromAfter, Currency: from.Currency},
		{WalletID: to.ID, Direction: DirectionCredit, Amount: amount, BalanceAfter: toAfter, Currency: to.Currency},
	}, nil
}

// LedgerRecord is the read model for the transaction history endpoint: one ledger
// row enriched with its journal context plus the counterparty wallet.
type LedgerRecord struct {
	EntryID              int64
	TransactionID        ID
	Type                 TransactionType
	Reference            string
	Direction            Direction
	Amount               Amount
	BalanceAfter         Amount
	Currency             Currency
	CounterpartyWalletID *ID
	CreatedAt            time.Time
}
