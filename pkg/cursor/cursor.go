// Package cursor encodes and decodes pagination cursors.
//
// The cursor is opaque (base64url of an internal value) so clients must not
// depend on its format: we can change the cursor strategy (e.g. switch to a
// (created_at, id) keyset) without a breaking change.
package cursor

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/example/wallet-service/pkg/apierror"
)

const entryPrefix = "e:"

// EncodeEntry builds a cursor from the last ledger entry id on the page.
func EncodeEntry(entryID int64) string {
	raw := entryPrefix + strconv.FormatInt(entryID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeEntry reads a cursor and returns the entry id as the upper bound
// (only entries with id < the result are returned).
// An unreadable cursor maps to 400 Bad Request.
func DecodeEntry(cursor string) (int64, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, badCursor()
	}
	raw := string(decoded)
	if !strings.HasPrefix(raw, entryPrefix) {
		return 0, badCursor()
	}
	value, err := strconv.ParseInt(strings.TrimPrefix(raw, entryPrefix), 10, 64)
	if err != nil || value <= 0 {
		return 0, badCursor()
	}
	return value, nil
}

func badCursor() error {
	return apierror.BadRequestFields("invalid cursor parameter",
		apierror.FieldError{Field: "cursor", Message: "the cursor must come from a previous response"})
}
