package domain

import "time"

// IdempotencyStatus is the processing state of an idempotency key.
type IdempotencyStatus string

const (
	// IdempotencyStatusInProgress: the claim is written, the business effect is not committed yet.
	IdempotencyStatusInProgress IdempotencyStatus = "in_progress"
	// IdempotencyStatusCompleted: the result is stored and may be replayed.
	IdempotencyStatusCompleted IdempotencyStatus = "completed"
)

// IdempotencyRecord is the outcome of the first execution of a mutating request.
// ResponseBody stores the serialized response so that a client retry returns an
// identical result instead of re-executing the business effect.
type IdempotencyRecord struct {
	Key          string
	Endpoint     string
	RequestHash  string
	Status       IdempotencyStatus
	StatusCode   int
	ResponseBody []byte
	ResourceID   *ID
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// ClaimResult is the outcome of an attempt to claim an idempotency key.
//
// Claimed=false means another request already used the key; Existing holds the
// first request's result, to be replayed or compared.
type ClaimResult struct {
	Claimed  bool
	Existing *IdempotencyRecord
}

const (
	idempotencyKeyMinLen = 8
	idempotencyKeyMaxLen = 200
)

// ValidateIdempotencyKey ensures the Idempotency-Key header is used sensibly.
// The minimum length prevents keys such as "1" or "test" that are easily
// reused by accident for a different request.
func ValidateIdempotencyKey(key string) error {
	if key == "" {
		return NewValidationError("Idempotency-Key", "header is required for mutating requests")
	}
	if len(key) < idempotencyKeyMinLen || len(key) > idempotencyKeyMaxLen {
		return NewValidationError("Idempotency-Key", "length must be between 8 and 200 characters")
	}
	for _, r := range key {
		// printable ASCII only: avoids odd keys coming from a malformed header.
		if r < 0x21 || r > 0x7e {
			return NewValidationError("Idempotency-Key", "must contain printable ASCII characters only")
		}
	}
	return nil
}
