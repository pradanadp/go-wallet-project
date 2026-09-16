package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example/wallet-service/pkg/apierror"
)

// decodeJSON reads a JSON body with strict discipline:
//   - the size is capped (MaxBytesReader) so one request cannot exhaust memory;
//   - unknown fields are rejected, so a typo such as "ammount_minor" is not
//     silently ignored and turned into a zero-value transaction;
//   - only a single JSON object is allowed (trailing data is rejected).
//
// Every decoding failure maps to 400 Bad Request, with field details when
// available.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBodyBytes int64) error {
	if r.Body == nil {
		return apierror.BadRequest("request body is required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return decodeError(err)
	}

	// Make sure there is no extra data after the first object.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apierror.BadRequest("the body may contain only one JSON object")
	}
	return nil
}

func decodeError(err error) error {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return apierror.BadRequest("request body exceeds the allowed size")
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return apierror.BadRequestFields("the body contains a value with the wrong type",
			apierror.FieldError{Field: field, Message: "value does not match the expected type"})
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return apierror.BadRequest("body is not valid JSON")
	}

	if errors.Is(err, io.EOF) {
		return apierror.BadRequest("request body is required")
	}

	// DisallowUnknownFields produces a plain text error whose message surface
	// is not sensitive, so it is safe to pass through to the client.
	return apierror.BadRequest(err.Error())
}
