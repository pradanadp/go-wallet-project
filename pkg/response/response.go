// Package response unifies how HTTP responses are written: success (a JSON resource
// or a page) and errors (RFC 7807), keeping the API contract consistent everywhere.
package response

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/reqctx"
)

const contentTypeJSON = "application/json; charset=utf-8"

// Pagination is the page metadata of a collection response.
type Pagination struct {
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
	Limit      int    `json:"limit"`
}

// Page is the standard collection response shape.
type Page[T any] struct {
	Data       []T        `json:"data"`
	Pagination Pagination `json:"pagination"`
}

// JSON writes a success payload with the given status code.
func JSON(w http.ResponseWriter, status int, payload any) {
	writeJSON(w, status, payload)
}

// RawJSON writes an already serialized body as-is.
// It is used to replay an idempotent response so its bytes are identical to the
// first response.
func RawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// Error writes an error using RFC 7807 problem details.
//
// 5xx errors are logged in full (including the cause) because only here do we know
// the context; 4xx does not need log noise.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	problem := apierror.From(err)
	problem.Type = problem.TypeURI()
	problem.Instance = r.URL.Path
	if problem.RequestID == "" {
		problem.RequestID = reqctx.RequestID(r.Context())
	}

	if problem.IsServerError() {
		reqctx.Logger(r.Context()).Error("request failed",
			slog.String("error", err.Error()),
			slog.String("code", problem.Code),
			slog.Any("cause", problem),
		)
	}

	writeJSON(w, problem.Status, problem)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(payload); err != nil {
		// The response has already been partially sent; the only correct action is to
		// log it. The status code can no longer be changed.
		slog.Error("failed to write the JSON response", slog.Any("error", err))
	}
}
