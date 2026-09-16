// Package domain contains the pure entities and business rules of the Wallet & Ledger
// Service. It deliberately imports nothing outside the standard library, so business
// rules cannot leak into infrastructure details (DB, HTTP, JSON) and can be tested
// without a database.
//
// Money convention: every value is stored as an Amount (int64) in minor units.
// Money arithmetic never uses floats.
package domain

import (
	"errors"
	"fmt"
)

// Domain sentinel errors. Every error returned by the usecase/repository layers must
// wrap one of these (via %w or Unwrap) so the HTTP layer can map it to a status
// code using errors.Is.
var (
	ErrNotFound            = errors.New("resource not found")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrWalletNotActive     = errors.New("wallet is not active")
	ErrCurrencyMismatch    = errors.New("currency mismatch")
	ErrSelfTransfer        = errors.New("transfer to the same wallet is not allowed")
	ErrIdempotencyConflict = errors.New("idempotency key already used with a different request")
	ErrIdempotencyInFlight = errors.New("another request with the same idempotency key is in progress")
	ErrForbidden           = errors.New("operation is not allowed")
	ErrValidation          = errors.New("validation error")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrRateLimited         = errors.New("rate limit exceeded")
	ErrUnavailable         = errors.New("service temporarily unavailable")
)

// ValidationError adds per-field context to ErrValidation so the handler can fill
// the `errors[]` array of an RFC 7807 problem details response.
type ValidationError struct {
	Field   string
	Message string
}

func NewValidationError(field, message string) *ValidationError {
	return &ValidationError{Field: field, Message: message}
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

func (e *ValidationError) Unwrap() error { return ErrValidation }

// NotFoundError is used when a resource does not exist.
type NotFoundError struct {
	Resource string
	ID       string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Resource, e.ID, ErrNotFound)
}

func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// InsufficientFundsError carries the current balance and the requested amount so
// the error message is informative without an extra query.
type InsufficientFundsError struct {
	WalletID ID
	Balance  Amount
	Required Amount
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("wallet %s: balance %d, required %d", e.WalletID, e.Balance, e.Required)
}

func (e *InsufficientFundsError) Unwrap() error { return ErrInsufficientFunds }

// WalletNotActiveError is returned for wallets in frozen/closed state.
type WalletNotActiveError struct {
	WalletID ID
	Status   WalletStatus
}

func (e *WalletNotActiveError) Error() string {
	return fmt.Sprintf("wallet %s: status %s", e.WalletID, e.Status)
}

func (e *WalletNotActiveError) Unwrap() error { return ErrWalletNotActive }

// CurrencyMismatchError prevents cross-currency transfers without conversion.
type CurrencyMismatchError struct {
	From Currency
	To   Currency
}

func (e *CurrencyMismatchError) Error() string {
	return fmt.Sprintf("currency %s does not match %s", e.From, e.To)
}

func (e *CurrencyMismatchError) Unwrap() error { return ErrCurrencyMismatch }

// SelfTransferError prevents transferring to the same wallet.
type SelfTransferError struct{ WalletID ID }

func (e *SelfTransferError) Error() string {
	return fmt.Sprintf("wallet %s: %s", e.WalletID, ErrSelfTransfer)
}

func (e *SelfTransferError) Unwrap() error { return ErrSelfTransfer }

// ForbiddenError is used when an operation is not allowed by business rules
// (for example, using the system wallet as a user wallet).
type ForbiddenError struct{ Reason string }

func (e *ForbiddenError) Error() string { return e.Reason }

func (e *ForbiddenError) Unwrap() error { return ErrForbidden }

// IdempotencyConflictError: same key but a different request payload.
type IdempotencyConflictError struct{ Key string }

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("idempotency key %q: %s", e.Key, ErrIdempotencyConflict)
}

func (e *IdempotencyConflictError) Unwrap() error { return ErrIdempotencyConflict }

// IdempotencyInFlightError: a twin request is still being processed.
type IdempotencyInFlightError struct{ Key string }

func (e *IdempotencyInFlightError) Error() string {
	return fmt.Sprintf("idempotency key %q: %s", e.Key, ErrIdempotencyInFlight)
}

func (e *IdempotencyInFlightError) Unwrap() error { return ErrIdempotencyInFlight }
