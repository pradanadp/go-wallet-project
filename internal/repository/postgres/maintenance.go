package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Maintenance holds housekeeping operations that run outside the request path.
type Maintenance struct {
	pool *pgxpool.Pool
}

func NewMaintenance(pool *pgxpool.Pool) *Maintenance { return &Maintenance{pool: pool} }

// PurgeExpiredIdempotencyKeys deletes idempotency keys whose TTL has passed.
//
// The TTL only controls cleanup, not how long the guarantee holds: a key that has
// not been deleted yet is still honoured (twin requests are still replayed), so a
// late janitor never causes a double execution.
func (m *Maintenance) PurgeExpiredIdempotencyKeys(ctx context.Context) (int64, error) {
	const query = `DELETE FROM idempotency_keys WHERE expires_at < now()`

	tag, err := m.pool.Exec(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("purging expired idempotency keys: %w", mapError(err))
	}
	return tag.RowsAffected(), nil
}
