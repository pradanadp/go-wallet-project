package postgres

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/wallet-service/internal/usecase"
	"github.com/example/wallet-service/pkg/reqctx"
)

// slowTransactionThreshold is the point at which a transaction counts as slow and
// gets logged. A long transaction holds row locks, so its duration is an important
// operational signal to monitor.
const slowTransactionThreshold = 200 * time.Millisecond

// UnitOfWork implements usecase.UnitOfWork with PostgreSQL transactions.
type UnitOfWork struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

func NewUnitOfWork(pool *pgxpool.Pool, log *slog.Logger) *UnitOfWork {
	return &UnitOfWork{pool: pool, log: log}
}

// WithTx runs fn inside a single READ COMMITTED transaction.
//
// READ COMMITTED is chosen (not SERIALIZABLE) because balance consistency is
// already guaranteed by explicit row locks: SERIALIZABLE would add random
// serialization failures that the application must retry, with no extra benefit
// for our access pattern of read-with-lock then write.
func (u *UnitOfWork) WithTx(ctx context.Context, fn func(ctx context.Context, repos usecase.Repos) error) error {
	startedAt := time.Now()

	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return mapError(err)
	}

	defer func() {
		// Rolling back after Commit is a no-op (ErrTxClosed) and is deliberately ignored.
		// context.WithoutCancel makes sure the rollback still runs even when the request
		// context has been cancelled (e.g. the client disconnected).
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := fn(ctx, newRepos(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return mapError(err)
	}

	if elapsed := time.Since(startedAt); elapsed > slowTransactionThreshold {
		reqctx.Logger(ctx).Warn("slow database transaction",
			slog.Duration("duration", elapsed),
			slog.String("threshold", slowTransactionThreshold.String()),
		)
	}
	return nil
}

// ReadRepos returns repositories without an explicit transaction, for read-only
// queries. A single PostgreSQL statement is already consistent (its own snapshot),
// so BEGIN/COMMIT would only add two round-trips.
func (u *UnitOfWork) ReadRepos() usecase.Repos {
	return newRepos(u.pool)
}
