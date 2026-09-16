package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/example/wallet-service/internal/domain"
)

type idempotencyRepository struct{ q querier }

// Claim tries to claim an idempotency key.
//
// INSERT ... ON CONFLICT DO NOTHING is chosen (instead of a plain INSERT plus
// catching the unique violation) because in PostgreSQL any error inside a
// transaction aborts that transaction: using ON CONFLICT keeps the "key already
// exists" path working without breaking the running transaction.
//
// Important: when another transaction has not committed the same key yet,
// ON CONFLICT DO NOTHING WAITS for that transaction to finish. This behaviour is
// what serializes twin requests at the database level, with no distributed
// lock.
func (r *idempotencyRepository) Claim(ctx context.Context, rec *domain.IdempotencyRecord) (domain.ClaimResult, error) {
	const insert = `
INSERT INTO idempotency_keys (key, endpoint, request_hash, status, created_at, updated_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $5, $6)
ON CONFLICT (key) DO NOTHING
RETURNING key`

	var claimedKey string
	err := r.q.QueryRow(ctx, insert,
		rec.Key,
		rec.Endpoint,
		rec.RequestHash,
		string(rec.Status),
		rec.CreatedAt,
		rec.ExpiresAt,
	).Scan(&claimedKey)

	switch {
	case err == nil:
		return domain.ClaimResult{Claimed: true}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// the key was already used by another request - read the first result.
	default:
		return domain.ClaimResult{}, mapError(err)
	}

	existing, err := r.getForUpdate(ctx, rec.Key)
	if err != nil {
		return domain.ClaimResult{}, err
	}
	return domain.ClaimResult{Claimed: false, Existing: existing}, nil
}

// getForUpdate reads the existing record and locks it, so the replay/conflict
// decision is based on state that can no longer change.
//
// Nullable columns are read via COALESCE(... ::text, ”) so their type is always
// non-NULL and scanning does not depend on pgx NULL handling.
func (r *idempotencyRepository) getForUpdate(ctx context.Context, key string) (*domain.IdempotencyRecord, error) {
	const query = `
SELECT key,
       endpoint,
       request_hash,
       status,
       COALESCE(status_code, 0),
       COALESCE(response_body::text, ''),
       COALESCE(resource_id::text, ''),
       created_at,
       expires_at
  FROM idempotency_keys
 WHERE key = $1
 FOR UPDATE`

	var (
		keyRaw       string
		endpoint     string
		requestHash  string
		status       string
		statusCode   int
		responseBody string
		resourceID   string
		createdAt    time.Time
		expiresAt    time.Time
	)

	err := r.q.QueryRow(ctx, query, key).
		Scan(&keyRaw, &endpoint, &requestHash, &status, &statusCode, &responseBody, &resourceID, &createdAt, &expiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Can happen when the janitor deletes the key right between the claim and the
			// read (very unlikely, but it must be handled so it does not become a 500).
			return nil, fmt.Errorf("%w: idempotency key %q disappeared while being read", domain.ErrNotFound, key)
		}
		return nil, mapError(err)
	}

	record := &domain.IdempotencyRecord{
		Key:         keyRaw,
		Endpoint:    endpoint,
		RequestHash: requestHash,
		Status:      domain.IdempotencyStatus(status),
		StatusCode:  statusCode,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
	}
	if responseBody != "" {
		record.ResponseBody = []byte(responseBody)
	}
	if resourceID != "" {
		id := domain.ID(resourceID)
		record.ResourceID = &id
	}
	return record, nil
}

// Complete stores the final response of the first request. This record is written
// in the same transaction as the business effect, so a "key already completed"
// state without a committed business effect is impossible.
func (r *idempotencyRepository) Complete(ctx context.Context, key string, statusCode int, body []byte, resourceID *domain.ID) error {
	const query = `
UPDATE idempotency_keys
   SET status        = $2,
       status_code   = $3,
       response_body = $4,
       resource_id   = $5,
       updated_at    = now()
 WHERE key = $1`

	var resource any
	if resourceID != nil {
		resource = resourceID.String()
	}

	tag, err := r.q.Exec(ctx, query, key, string(domain.IdempotencyStatusCompleted), statusCode, body, resource)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: idempotency key %q not found while storing the response", domain.ErrNotFound, key)
	}
	return nil
}
