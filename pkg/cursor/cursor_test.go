package cursor_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/cursor"
)

func TestEncodeDecodeEntryRoundTrip(t *testing.T) {
	for _, entryID := range []int64{1, 42, 9_999_999, 1 << 40} {
		encoded := cursor.EncodeEntry(entryID)

		decoded, err := cursor.DecodeEntry(encoded)
		if err != nil {
			t.Fatalf("DecodeEntry(%q): %v", encoded, err)
		}
		if decoded != entryID {
			t.Errorf("decoded = %d, want %d", decoded, entryID)
		}
	}
}

func TestEncodeEntryIsOpaque(t *testing.T) {
	encoded := cursor.EncodeEntry(42)

	if encoded == "42" {
		t.Fatal("the cursor must not leak the internal value directly")
	}
	if strings.ContainsAny(encoded, "=+/") {
		t.Errorf("cursor %q is not URL-safe", encoded)
	}
}

func TestDecodeEntryRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "not base64", input: "!!!not-a-cursor!!!"},
		{name: "valid base64 but wrong prefix", input: "eDox"},
		{name: "right prefix but the value is not a number", input: "ZTphYmM"},
		{name: "zero value", input: "ZTow"},
		{name: "negative value", input: "ZTotMQ"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cursor.DecodeEntry(tc.input)
			if err == nil {
				t.Fatal("error is nil, want 400 bad_request")
			}

			// A cursor is a transport parameter, so a failure is a malformed request (400),
			// not a business rule violation (422).
			var apiErr *apierror.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *apierror.Error", err, err)
			}
			if apiErr.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", apiErr.Status)
			}
			if apiErr.Code != apierror.CodeBadRequest {
				t.Errorf("code = %q, want %q", apiErr.Code, apierror.CodeBadRequest)
			}
		})
	}
}
