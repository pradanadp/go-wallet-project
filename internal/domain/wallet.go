package domain

import "time"

// WalletStatus is the wallet lifecycle state.
type WalletStatus string

const (
	WalletStatusActive WalletStatus = "active"
	WalletStatusFrozen WalletStatus = "frozen"
	WalletStatusClosed WalletStatus = "closed"
)

func ParseWalletStatus(field, raw string) (WalletStatus, error) {
	switch WalletStatus(raw) {
	case WalletStatusActive, WalletStatusFrozen, WalletStatusClosed:
		return WalletStatus(raw), nil
	default:
		return "", NewValidationError(field, "unknown wallet status")
	}
}

// Wallet is a user's wallet. The business rules attached to this entity are the
// only place where a balance may change.
//
// is_system marks the "world account" that represents money outside the system;
// the system wallet may run negative because its balance mirrors the total money
// already circulated to users.
type Wallet struct {
	ID        ID
	OwnerID   ID
	Currency  Currency
	Status    WalletStatus
	IsSystem  bool
	Balance   Amount
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewWallet creates an active user wallet with a zero balance.
func NewWallet(ownerID ID, currency Currency, now time.Time) *Wallet {
	return &Wallet{
		OwnerID:   ownerID,
		Currency:  currency,
		Status:    WalletStatusActive,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
}

func (w *Wallet) IsActive() bool { return w.Status == WalletStatusActive }

// ensurePostable checks that the wallet may receive balance mutations.
func (w *Wallet) ensurePostable() error {
	if !w.IsActive() {
		return &WalletNotActiveError{WalletID: w.ID, Status: w.Status}
	}
	return nil
}

// Credit adds to the balance and returns the balance after the mutation.
func (w *Wallet) Credit(amount Amount) (Amount, error) {
	if err := w.ensurePostable(); err != nil {
		return 0, err
	}
	w.Balance += amount
	w.Version++
	return w.Balance, nil
}

// Debit subtracts from the balance and returns the balance after the mutation.
// A non-system wallet must never go negative; the system wallet may.
func (w *Wallet) Debit(amount Amount) (Amount, error) {
	if err := w.ensurePostable(); err != nil {
		return 0, err
	}
	if !w.IsSystem && w.Balance < amount {
		return 0, &InsufficientFundsError{WalletID: w.ID, Balance: w.Balance, Required: amount}
	}
	w.Balance -= amount
	w.Version++
	return w.Balance, nil
}

func (w *Wallet) ensureOperable() error {
	if err := w.ensurePostable(); err != nil {
		return err
	}
	if w.IsSystem {
		return &ForbiddenError{Reason: "the system wallet cannot be operated on directly"}
	}
	return nil
}
