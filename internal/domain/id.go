package domain

import (
	"regexp"
	"strings"
)

// ID is an opaque UUID-based identifier (RFC 4122 format). It is a named string
// type so it cannot be confused with other identifiers (for example an
// idempotency key, which is also a string).
type ID string

// NilID is the zero UUID, used as the owner of the system wallet.
const NilID ID = "00000000-0000-0000-0000-000000000000"

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseID validates a string and normalizes it (lowercase) into an ID.
// field is used in the validation message, e.g. "wallet_id".
func ParseID(field, raw string) (ID, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if !uuidPattern.MatchString(v) {
		return "", NewValidationError(field, "must be a valid UUID")
	}
	return ID(v), nil
}

// MustParseID is only used for constant values that are known to be valid
// (for example a config default). A panic here means a programmer bug.
func MustParseID(raw string) ID {
	id, err := ParseID("id", raw)
	if err != nil {
		panic(err)
	}
	return id
}

func (id ID) String() string { return string(id) }

// IsZero reports whether the ID is empty (not set yet).
func (id ID) IsZero() bool { return id == "" }
