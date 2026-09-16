package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/example/wallet-service/internal/domain"
)

// statusOK is the status code stored for a successful mutating request
// (a literal is used so the usecase does not need to import net/http).
const statusOK = 200

// idempotentSpec describes a request protected by an idempotency key.
type idempotentSpec struct {
	key         string
	endpoint    string
	requestHash string
	statusCode  int
	// optionalKey=true means the endpoint may still be processed without an
	// Idempotency-Key (e.g. wallet creation), whereas money movement endpoints
	// require it.
	optionalKey bool
}

// hashRequest builds a deterministic fingerprint of the request fields.
// It distinguishes "the same retry" (replayed) from
// "the key reused for a different request" (409 conflict).
func hashRequest(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0x1f}) // separator so that ("ab","c") != ("a","bc")
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// runIdempotent runs exec with an at-most-once guarantee for a given
// (key, requestHash) pair.
//
// Flow:
//  1. claim the key inside the same database transaction as the business effect;
//  2. if the key already exists -> replay the stored response (replayed=true),
//     or return conflict/in-flight when the payload differs or is still running;
//  3. if the claim succeeds -> run the business effect, store the response, commit.
//
// Because the claim and the business effect share one transaction, a business
// failure (e.g. insufficient balance) rolls the claim back so the key can be reused,
// while a twin request arriving at the same time waits at the database level
// (unique index) until the first transaction finishes - with no distributed lock.
//
// Returns: the result, whether it was replayed, and an error.
func runIdempotent[T any](
	ctx context.Context,
	uow UnitOfWork,
	clock Clock,
	ttl time.Duration,
	spec idempotentSpec,
	exec func(ctx context.Context, repos Repos) (*T, string, error),
) (*T, bool, error) {
	if spec.key == "" && spec.optionalKey {
		// This endpoint may be called without an Idempotency-Key (no anti-duplication
		// guarantee), but it still runs inside a single transaction.
		var out *T
		err := uow.WithTx(ctx, func(ctx context.Context, repos Repos) error {
			view, _, err := exec(ctx, repos)
			out = view
			return err
		})
		return out, false, err
	}
	if err := domain.ValidateIdempotencyKey(spec.key); err != nil {
		return nil, false, err
	}

	var (
		out      *T
		replayed bool
	)
	err := uow.WithTx(ctx, func(ctx context.Context, repos Repos) error {
		claim, err := repos.Idempotency().Claim(ctx, &domain.IdempotencyRecord{
			Key:         spec.key,
			Endpoint:    spec.endpoint,
			RequestHash: spec.requestHash,
			Status:      domain.IdempotencyStatusInProgress,
			CreatedAt:   clock().UTC(),
			ExpiresAt:   clock().UTC().Add(ttl),
		})
		if err != nil {
			return err
		}

		if !claim.Claimed {
			existing, err := replayStored[T](claim.Existing, spec.endpoint, spec.requestHash)
			if err != nil {
				return err
			}
			out, replayed = &existing, true
			return nil
		}

		view, resourceID, err := exec(ctx, repos)
		if err != nil {
			return err
		}

		body, err := json.Marshal(view)
		if err != nil {
			return fmt.Errorf("idempotency: failed to store the response for key %q: %w", spec.key, err)
		}
		var rid *domain.ID
		if resourceID != "" {
			id := domain.ID(resourceID)
			rid = &id
		}
		if err := repos.Idempotency().Complete(ctx, spec.key, spec.statusCode, body, rid); err != nil {
			return err
		}
		out = view
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, replayed, nil
}

// replayStored re-validates an existing key and loads the stored response back
// into its original struct. The response is guaranteed to match the first
// execution because what is stored is the json.Marshal output of the same struct.
func replayStored[T any](existing *domain.IdempotencyRecord, endpoint, requestHash string) (T, error) {
	var zero T
	if existing == nil {
		return zero, fmt.Errorf("idempotency: no record found for the first request")
	}
	if existing.Endpoint != endpoint || existing.RequestHash != requestHash {
		return zero, &domain.IdempotencyConflictError{Key: existing.Key}
	}
	if existing.Status != domain.IdempotencyStatusCompleted {
		// Practically unobservable: the claim and the business effect commit together, so
		// an in_progress row visible to another transaction is only possible if the claim
		// were deliberately committed on its own (not done in this service). It is
		// handled anyway to stay safe.
		return zero, &domain.IdempotencyInFlightError{Key: existing.Key}
	}
	var out T
	if err := json.Unmarshal(existing.ResponseBody, &out); err != nil {
		return zero, fmt.Errorf("idempotency: failed to read the stored response: %w", err)
	}
	return out, nil
}

// normalizeReference cleans up the optional client reference.
func normalizeReference(reference string) (string, error) {
	const maxReferenceLen = 128
	if len(reference) > maxReferenceLen {
		return "", domain.NewValidationError("reference", fmt.Sprintf("maksimum %d karakter", maxReferenceLen))
	}
	return strings.TrimSpace(reference), nil
}
