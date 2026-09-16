// Package postgres is the database adapter for the ports declared by the usecase
// layer. Every query here is explicit (no ORM) because details such as FOR UPDATE,
// lock ordering, and ON CONFLICT are the very core of this system's correctness -
// hiding them behind an ORM would take away control over concurrency behaviour.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/example/wallet-service/internal/domain"
	"github.com/example/wallet-service/internal/usecase"
)

// PostgreSQL error codes that need translation into domain errors.
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	pgErrCheckViolation = "23514"
	pgErrForeignKey     = "23503"
	pgErrSerialization  = "40001"
	pgErrDeadlock       = "40P01"
)

// Constraints with special business meaning.
const (
	constraintBalanceGuard = "wallets_balance_guard"
	constraintOwnerFK      = "wallets_owner_id_fkey"
)

// querier is the set of methods shared by pgxpool.Pool and pgx.Tx.
// Thanks to it, the same repository works inside and outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// repos implements usecase.Repos.
type repos struct{ q querier }

func newRepos(q querier) *repos { return &repos{q: q} }

func (r *repos) Wallets() usecase.WalletRepository { return &walletRepository{q: r.q} }

func (r *repos) Ledger() usecase.LedgerRepository { return &ledgerRepository{q: r.q} }

func (r *repos) Idempotency() usecase.IdempotencyRepository { return &idempotencyRepository{q: r.q} }

// mapError translates driver errors into domain errors, so upper layers never
// know about pgconn/pgx and can use errors.Is with domain sentinels. Unknown
// errors are passed through as-is (becoming a 500).
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Used as a "no rows" signal; the caller, which knows the resource,
		// wraps it into an informative NotFoundError.
		return domain.ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCheckViolation:
			if pgErr.ConstraintName == constraintBalanceGuard {
				// Last line of defence: the application should already have rejected
				// this, so reaching it signals a balance-logic bug.
				return fmt.Errorf("%w: constraint %s violated", domain.ErrInsufficientFunds, constraintBalanceGuard)
			}
			if pgErr.ConstraintName == constraintOwnerFK {
				return fmt.Errorf("%w: owner_id not found", domain.ErrNotFound)
			}
			return fmt.Errorf("%w: constraint %s violated", domain.ErrValidation, pgErr.ConstraintName)
		case pgErrForeignKey:
			return fmt.Errorf("%w: referenced data not found (%s)", domain.ErrNotFound, pgErr.ConstraintName)
		case pgErrSerialization, pgErrDeadlock:
			// Not expected for our access pattern (everything goes through the primary key
			// with canonical lock ordering), but still mapped to 503 so the client knows
			// the request is worth retrying.
			return fmt.Errorf("%w: %s", domain.ErrUnavailable, pgErr.Message)
		}
		return fmt.Errorf("database error %s: %s", pgErr.Code, pgErr.Message)
	}

	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return fmt.Errorf("%w: %v", domain.ErrUnavailable, err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: request cancelled", domain.ErrUnavailable)
	}
	// context.DeadlineExceeded is deliberately left untouched so it maps to
	// 504 Request Timeout instead of 503.
	return err
}
