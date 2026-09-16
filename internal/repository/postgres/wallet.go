package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/example/wallet-service/internal/domain"
)

type walletRepository struct{ q querier }

const walletColumns = `id, owner_id, currency, status, is_system, balance_minor, version, created_at, updated_at`

func (r *walletRepository) Create(ctx context.Context, w *domain.Wallet) error {
	const query = `
INSERT INTO wallets (id, owner_id, currency, status, is_system, balance_minor, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	// The ID is generated in the application (not by gen_random_uuid()) so the usecase
	// can return the resource identity without an extra round-trip and tests can use
	// deterministic IDs.
	_, err := r.q.Exec(ctx, query,
		w.ID.String(),
		w.OwnerID.String(),
		w.Currency.String(),
		string(w.Status),
		w.IsSystem,
		w.Balance.Int64(),
		w.Version,
		w.CreatedAt,
		w.UpdatedAt,
	)
	return mapError(err)
}

func (r *walletRepository) GetByID(ctx context.Context, id domain.ID) (*domain.Wallet, error) {
	return r.get(ctx, id, false)
}

// GetByIDForUpdate locks the wallet row until the transaction ends.
//
// The lock is always taken through the primary key (WHERE id = $1) so PostgreSQL
// locks a single row instead of scanning the table.
func (r *walletRepository) GetByIDForUpdate(ctx context.Context, id domain.ID) (*domain.Wallet, error) {
	return r.get(ctx, id, true)
}

func (r *walletRepository) get(ctx context.Context, id domain.ID, forUpdate bool) (*domain.Wallet, error) {
	query := `SELECT ` + walletColumns + ` FROM wallets WHERE id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}

	var (
		idRaw      string
		ownerIDRaw string
		currency   string
		status     string
		isSystem   bool
		balance    int64
		version    int64
		createdAt  time.Time
		updatedAt  time.Time
	)

	// Scan into primitive types and convert explicitly: this avoids relying on pgx
	// reflection for named types (domain.ID, domain.Amount), so the behaviour is
	// predictable.
	err := r.q.QueryRow(ctx, query, id.String()).
		Scan(&idRaw, &ownerIDRaw, &currency, &status, &isSystem, &balance, &version, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &domain.NotFoundError{Resource: "wallet", ID: id.String()}
		}
		return nil, mapError(err)
	}

	return &domain.Wallet{
		ID:        domain.ID(idRaw),
		OwnerID:   domain.ID(ownerIDRaw),
		Currency:  domain.Currency(currency),
		Status:    domain.WalletStatus(status),
		IsSystem:  isSystem,
		Balance:   domain.Amount(balance),
		Version:   version,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func (r *walletRepository) Update(ctx context.Context, w *domain.Wallet) error {
	const query = `
UPDATE wallets
   SET balance_minor = $2,
       status        = $3,
       version       = $4,
       updated_at    = $5
 WHERE id = $1`

	tag, err := r.q.Exec(ctx, query, w.ID.String(), w.Balance.Int64(), string(w.Status), w.Version, w.UpdatedAt)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		// The row vanished mid-transaction: this should not happen because we hold the
		// row lock, so it is a condition that deserves to fail.
		return &domain.NotFoundError{Resource: "wallet", ID: w.ID.String()}
	}
	return nil
}
