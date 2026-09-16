// Package usecase contains the application logic (orchestrating domain + repositories).
//
// Every outbound dependency is declared as a PORT interface in this package
// (dependency inversion), which means:
//   - the usecase knows nothing about PostgreSQL, HTTP, or JSON;
//   - the usecase can be fully tested with in-memory fakes.
package usecase

import (
	"context"
	"time"

	"github.com/example/wallet-service/internal/domain"
)

// Clock allows time to be injected in tests (avoids time.Sleep/flaky tests).
type Clock func() time.Time

// IDGenerator produces new identities. It is a port so tests can use
// deterministic IDs. Production implementation: pkg/uuid.V4.
type IDGenerator func() (string, error)

// Repos is a set of repositories bound to a single database transaction.
// The usecase only sees this abstraction, never *pgx.Tx.
type Repos interface {
	Wallets() WalletRepository
	Ledger() LedgerRepository
	Idempotency() IdempotencyRepository
}

// UnitOfWork controls transaction boundaries. It is the only way the usecase
// can express "all of the following must commit together or not at all".
type UnitOfWork interface {
	// WithTx runs fn inside a single database transaction. If fn returns an error,
	// the transaction is rolled back.
	WithTx(ctx context.Context, fn func(ctx context.Context, repos Repos) error) error
	// ReadRepos returns repositories without an explicit transaction, for read-only queries.
	ReadRepos() Repos
}

// WalletRepository is the wallet data access port.
type WalletRepository interface {
	Create(ctx context.Context, w *domain.Wallet) error
	GetByID(ctx context.Context, id domain.ID) (*domain.Wallet, error)
	// GetByIDForUpdate locks the wallet row (SELECT ... FOR UPDATE) until the
	// transaction ends. The usecase must call it in sorted ID order to prevent
	// deadlocks.
	GetByIDForUpdate(ctx context.Context, id domain.ID) (*domain.Wallet, error)
	Update(ctx context.Context, w *domain.Wallet) error
}

// LedgerRepository is the journal & ledger data access port.
type LedgerRepository interface {
	CreateTransaction(ctx context.Context, t *domain.Transaction) error
	InsertEntries(ctx context.Context, entries []domain.Entry) error
	// ListRecordsByWallet returns one wallet's ledger history, newest first.
	// afterEntryID > 0 means "only entries with id < afterEntryID" (cursor).
	ListRecordsByWallet(ctx context.Context, walletID domain.ID, afterEntryID int64, limit int) ([]domain.LedgerRecord, error)
}

// IdempotencyRepository is the idempotency key storage port.
type IdempotencyRepository interface {
	// Claim tries to store a new key. When the key already exists (twin request),
	// Claim returns Claimed=false plus the first request's record and does NOT
	// treat it as an error.
	Claim(ctx context.Context, rec *domain.IdempotencyRecord) (domain.ClaimResult, error)
	// Complete stores the final response of a successful request.
	Complete(ctx context.Context, key string, statusCode int, body []byte, resourceID *domain.ID) error
}
