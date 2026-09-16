// Package apierror maps errors from every layer into a single consistent error
// response shape: RFC 7807 "Problem Details for HTTP APIs".
//
// The mapping is centralized here so that:
//   - domain/usecase stays unaware of HTTP status codes;
//   - every endpoint automatically produces the same error shape.
package apierror

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/example/wallet-service/internal/domain"
)

// TypeBase is the URI prefix for the `type` field of problem details.
const TypeBase = "https://api.example.com/problems/"

// Machine-readable error codes (stable, also used as the slug in the `type` field).
const (
	CodeBadRequest            = "bad_request"
	CodeUnauthorized          = "unauthorized"
	CodeForbidden             = "forbidden"
	CodeNotFound              = "not_found"
	CodeMethodNotAllowed      = "method_not_allowed"
	CodeInsufficientFunds     = "insufficient_funds"
	CodeWalletNotActive       = "wallet_not_active"
	CodeIdempotencyConflict   = "idempotency_conflict"
	CodeIdempotencyInProgress = "idempotency_in_progress"
	CodeCurrencyMismatch      = "currency_mismatch"
	CodeSelfTransfer          = "self_transfer"
	CodeValidation            = "validation_error"
	CodeRateLimited           = "rate_limited"
	CodeTimeout               = "request_timeout"
	CodeInternal              = "internal_error"
	CodeUnavailable           = "database_unavailable"
)

// FieldError describes a single validation violation (an RFC 7807 extension).
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Problem is the RFC 7807 representation plus the `code`, `request_id`, and `errors` extensions.
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Instance  string       `json:"instance,omitempty"`
	Code      string       `json:"code"`
	RequestID string       `json:"request_id,omitempty"`
	Errors    []FieldError `json:"errors,omitempty"`

	// cause is not serialized (unexported); it is used for logging and errors.Is.
	cause error
}

func (p *Problem) Error() string { return fmt.Sprintf("%d %s: %s", p.Status, p.Code, p.Detail) }

// Unwrap lets errors.Is/As reach through the problem to the original error.
func (p *Problem) Unwrap() error { return p.cause }

// Error is a transport-level error (a 4xx/5xx built directly by the HTTP layer).
// It implements error, so it can be returned from a handler and mapped by From()
// exactly like a domain error.
type Error struct {
	Status int
	Code   string
	Title  string
	Detail string
	Fields []FieldError
	cause  error
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Detail) }

func (e *Error) Unwrap() error { return e.cause }

// New builds a transport error with a detail message.
func New(status int, code, title, detail string) *Error {
	return &Error{Status: status, Code: code, Title: title, Detail: detail}
}

// BadRequest is used for a body/query/header that cannot be processed.
func BadRequest(detail string) *Error {
	return New(http.StatusBadRequest, CodeBadRequest, "Bad Request", detail)
}

// BadRequestFields is used for input validation in the HTTP layer.
func BadRequestFields(detail string, fields ...FieldError) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeBadRequest, Title: "Bad Request", Detail: detail, Fields: fields}
}

// MethodNotAllowed is used by the fallback route.
func MethodNotAllowed(method string) *Error {
	return New(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "Method Not Allowed",
		fmt.Sprintf("method %s is not supported on this endpoint", method))
}

// InternalPanic is used by the Recover middleware: technical details are never
// sent to the client, only written to the log.
func InternalPanic() *Error {
	return New(http.StatusInternalServerError, CodeInternal, "Internal Server Error", "an internal error occurred")
}

// Unauthorized is used by the Auth middleware.
func Unauthorized(detail string) *Error {
	return New(http.StatusUnauthorized, CodeUnauthorized, "Unauthorized", detail)
}

// RateLimited is used by the RateLimit middleware.
func RateLimited() *Error {
	return New(http.StatusTooManyRequests, CodeRateLimited, "Too Many Requests",
		"too many requests from this address, try again shortly")
}

// Unavailable is used by the readiness probe and unhealthy dependencies.
func Unavailable(detail string) *Error {
	return New(http.StatusServiceUnavailable, CodeUnavailable, "Service Unavailable", detail)
}

// NotFoundEndpoint is used for an unknown path.
func NotFoundEndpoint(path string) *Error {
	return New(http.StatusNotFound, CodeNotFound, "Not Found", fmt.Sprintf("endpoint %s was not found", path))
}

// Wrap turns any error into a problem with a given status/code while preserving
// the original error chain (errors.Is/As keep working).
func Wrap(err error, status int, code, title, detail string) *Problem {
	return &Problem{Status: status, Code: code, Title: title, Detail: detail, cause: err}
}

// From is the single place where errors are translated into an HTTP response.
//
// The order of checks matters: transport errors first, then domain sentinels.
// Every check uses errors.Is/errors.As, so an error wrapped with %w in any layer
// is still mapped correctly.
func From(err error) *Problem {
	if err == nil {
		return Wrap(nil, http.StatusInternalServerError, CodeInternal, "Internal Server Error", "")
	}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		return &Problem{
			Status: apiErr.Status,
			Code:   apiErr.Code,
			Title:  apiErr.Title,
			Detail: apiErr.Detail,
			Errors: apiErr.Fields,
			cause:  err,
		}
	}

	var problem *Problem
	if errors.As(err, &problem) {
		if problem.cause == nil {
			problem.cause = err
		}
		return problem
	}

	switch {
	case errors.Is(err, domain.ErrValidation):
		problem := Wrap(err, http.StatusUnprocessableEntity, CodeValidation, "Unprocessable Entity", err.Error())
		var validationErr *domain.ValidationError
		if errors.As(err, &validationErr) {
			problem.Detail = validationErr.Message
			problem.Errors = []FieldError{{Field: validationErr.Field, Message: validationErr.Message}}
		}
		return problem

	case errors.Is(err, domain.ErrNotFound):
		return Wrap(err, http.StatusNotFound, CodeNotFound, "Not Found", err.Error())

	case errors.Is(err, domain.ErrInsufficientFunds):
		return Wrap(err, http.StatusConflict, CodeInsufficientFunds, "Insufficient Funds", err.Error())

	case errors.Is(err, domain.ErrWalletNotActive):
		return Wrap(err, http.StatusConflict, CodeWalletNotActive, "Wallet Not Active", err.Error())

	case errors.Is(err, domain.ErrIdempotencyConflict):
		return Wrap(err, http.StatusConflict, CodeIdempotencyConflict, "Idempotency Key Conflict",
			"this Idempotency-Key was already used for a request with a different payload")

	case errors.Is(err, domain.ErrIdempotencyInFlight):
		return Wrap(err, http.StatusConflict, CodeIdempotencyInProgress, "Idempotency Request In Progress",
			"a request with this Idempotency-Key is still being processed")

	case errors.Is(err, domain.ErrCurrencyMismatch):
		return Wrap(err, http.StatusUnprocessableEntity, CodeCurrencyMismatch, "Currency Mismatch", err.Error())

	case errors.Is(err, domain.ErrSelfTransfer):
		return Wrap(err, http.StatusUnprocessableEntity, CodeSelfTransfer, "Self Transfer Not Allowed", err.Error())

	case errors.Is(err, domain.ErrForbidden):
		return Wrap(err, http.StatusForbidden, CodeForbidden, "Forbidden", err.Error())

	case errors.Is(err, domain.ErrUnauthorized):
		return Wrap(err, http.StatusUnauthorized, CodeUnauthorized, "Unauthorized", err.Error())

	case errors.Is(err, domain.ErrRateLimited):
		return Wrap(err, http.StatusTooManyRequests, CodeRateLimited, "Too Many Requests", err.Error())

	case errors.Is(err, context.DeadlineExceeded):
		return Wrap(err, http.StatusGatewayTimeout, CodeTimeout, "Request Timeout", "the request exceeded its time budget")

	case errors.Is(err, domain.ErrUnavailable):
		return Wrap(err, http.StatusServiceUnavailable, CodeUnavailable, "Service Unavailable", err.Error())
	}

	// Unexpected error: never leak internal details to the client, but still record
	// them in full by the caller (logging middleware).
	return Wrap(err, http.StatusInternalServerError, CodeInternal, "Internal Server Error",
		"an internal error occurred")
}

// IsServerError marks a problem that must be logged as an error.
func (p *Problem) IsServerError() bool { return p.Status >= http.StatusInternalServerError }

func (p *Problem) TypeURI() string {
	if p.Type != "" {
		return p.Type
	}
	return TypeBase + p.Code
}
