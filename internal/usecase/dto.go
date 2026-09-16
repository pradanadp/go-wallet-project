package usecase

import "time"

// Application-layer DTOs / response models.
//
// These models live in the usecase layer (not in the handler) because idempotency
// stores a serialized success response, so the usecase must own a stable shape
// that can be replayed. The handler simply encodes this struct as-is, which
// guarantees that the first response and a replay are identical.

const (
	// DefaultPageLimit is used when the client does not send ?limit=.
	DefaultPageLimit = 20
	// MaxPageLimit caps the rows per page so a single request cannot pull the
	// whole ledger.
	MaxPageLimit = 100
)

// WalletView is the API representation of a wallet.
type WalletView struct {
	ID           string    `json:"id"`
	OwnerID      string    `json:"owner_id"`
	Currency     string    `json:"currency"`
	Status       string    `json:"status"`
	BalanceMinor int64     `json:"balance_minor"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// Replayed is only relevant for idempotency and is never serialized.
	Replayed bool `json:"-"`
}

// MovementView is the outcome of one money movement (top-up / withdraw / transfer).
type MovementView struct {
	TransactionID string    `json:"transaction_id"`
	Type          string    `json:"type"`
	Status        string    `json:"status"`
	WalletID      string    `json:"wallet_id"`
	AmountMinor   int64     `json:"amount_minor"`
	Currency      string    `json:"currency"`
	BalanceAfter  int64     `json:"balance_after_minor"`
	Reference     string    `json:"reference,omitempty"`
	CreatedAt     time.Time `json:"created_at"`

	// CounterpartyWalletID is set for transfers (the destination wallet).
	CounterpartyWalletID string `json:"counterparty_wallet_id,omitempty"`

	// Replayed marks a response that was replayed by idempotency.
	Replayed bool `json:"-"`
}

// LedgerRecordView is one row of a wallet's transaction history.
type LedgerRecordView struct {
	EntryID              int64     `json:"entry_id"`
	TransactionID        string    `json:"transaction_id"`
	Type                 string    `json:"type"`
	Direction            string    `json:"direction"`
	AmountMinor          int64     `json:"amount_minor"`
	BalanceAfterMinor    int64     `json:"balance_after_minor"`
	Currency             string    `json:"currency"`
	Reference            string    `json:"reference,omitempty"`
	CounterpartyWalletID string    `json:"counterparty_wallet_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// TransactionPage is one page of transaction history.
// NextEntryID is used by the handler to build the next page cursor.
type TransactionPage struct {
	Records     []LedgerRecordView
	NextEntryID int64
	HasMore     bool
}
