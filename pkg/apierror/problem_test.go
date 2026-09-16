package apierror_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/example/wallet-service/internal/domain"
	"github.com/example/wallet-service/pkg/apierror"
)

const walletID = "11111111-1111-4111-8111-111111111111"

// TestFromMapsDomainErrors verifies the most important contract of this service:
// domain error -> HTTP status + machine code, through errors.Is/errors.As.
func TestFromMapsDomainErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "validation",
			err:        domain.NewValidationError("amount_minor", "must be greater than 0"),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierror.CodeValidation,
		},
		{
			name:       "not found",
			err:        &domain.NotFoundError{Resource: "wallet", ID: walletID},
			wantStatus: http.StatusNotFound,
			wantCode:   apierror.CodeNotFound,
		},
		{
			name:       "insufficient funds",
			err:        &domain.InsufficientFundsError{WalletID: walletID, Balance: 1_000, Required: 5_000},
			wantStatus: http.StatusConflict,
			wantCode:   apierror.CodeInsufficientFunds,
		},
		{
			name:       "wallet not active",
			err:        &domain.WalletNotActiveError{WalletID: walletID, Status: domain.WalletStatusFrozen},
			wantStatus: http.StatusConflict,
			wantCode:   apierror.CodeWalletNotActive,
		},
		{
			name:       "idempotency conflict",
			err:        &domain.IdempotencyConflictError{Key: "abc"},
			wantStatus: http.StatusConflict,
			wantCode:   apierror.CodeIdempotencyConflict,
		},
		{
			name:       "idempotency in progress",
			err:        &domain.IdempotencyInFlightError{Key: "abc"},
			wantStatus: http.StatusConflict,
			wantCode:   apierror.CodeIdempotencyInProgress,
		},
		{
			name:       "currency mismatch",
			err:        &domain.CurrencyMismatchError{From: "IDR", To: "USD"},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierror.CodeCurrencyMismatch,
		},
		{
			name:       "self transfer",
			err:        &domain.SelfTransferError{WalletID: walletID},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierror.CodeSelfTransfer,
		},
		{
			name:       "forbidden operation",
			err:        &domain.ForbiddenError{Reason: "system wallet"},
			wantStatus: http.StatusForbidden,
			wantCode:   apierror.CodeForbidden,
		},
		{
			name:       "unauthenticated",
			err:        domain.ErrUnauthorized,
			wantStatus: http.StatusUnauthorized,
			wantCode:   apierror.CodeUnauthorized,
		},
		{
			name:       "rate limited",
			err:        domain.ErrRateLimited,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   apierror.CodeRateLimited,
		},
		{
			name:       "database unavailable",
			err:        fmt.Errorf("%w: connection refused", domain.ErrUnavailable),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierror.CodeUnavailable,
		},
		{
			name:       "request timeout",
			err:        fmt.Errorf("query cancelled: %w", context.DeadlineExceeded),
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   apierror.CodeTimeout,
		},
		{
			name:       "unknown error becomes 500",
			err:        errors.New("index out of range"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierror.CodeInternal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problem := apierror.From(tc.err)

			if problem.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", problem.Status, tc.wantStatus)
			}
			if problem.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", problem.Code, tc.wantCode)
			}
			if problem.Title == "" {
				t.Error("title is empty")
			}
			if problem.TypeURI() != apierror.TypeBase+tc.wantCode {
				t.Errorf("type = %q", problem.TypeURI())
			}
			if !errors.Is(problem, tc.err) {
				t.Errorf("errors.Is(problem, original err) = false, the error chain is broken")
			}
		})
	}
}

func TestFromMarksServerErrors(t *testing.T) {
	if apierror.From(errors.New("boom")).IsServerError() != true {
		t.Error("an unknown error must be flagged as a server error")
	}
	if apierror.From(domain.ErrNotFound).IsServerError() != false {
		t.Error("404 is not a server error")
	}
}

func TestFromDoesNotLeakInternalDetail(t *testing.T) {
	problem := apierror.From(errors.New("panic: db connection user=wallet password=secret"))

	if problem.Detail != "an internal error occurred" {
		t.Errorf("detail = %q, internal detail leaked to the client", problem.Detail)
	}
	if problem.Title != "Internal Server Error" {
		t.Errorf("title = %q", problem.Title)
	}
}

func TestValidationErrorFillsFieldErrors(t *testing.T) {
	problem := apierror.From(domain.NewValidationError("limit", "must be between 1 and 100"))

	if len(problem.Errors) != 1 {
		t.Fatalf("errors = %+v, want exactly 1 field error", problem.Errors)
	}
	if problem.Errors[0].Field != "limit" {
		t.Errorf("field = %q, want limit", problem.Errors[0].Field)
	}
	if problem.Detail != "must be between 1 and 100" {
		t.Errorf("detail = %q", problem.Detail)
	}
}

func TestTransportErrorsPreserveStatusAndFields(t *testing.T) {
	transportErr := apierror.BadRequestFields("invalid body",
		apierror.FieldError{Field: "Idempotency-Key", Message: "is required"})

	problem := apierror.From(fmt.Errorf("handler: %w", transportErr))

	if problem.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", problem.Status)
	}
	if problem.Code != apierror.CodeBadRequest {
		t.Errorf("code = %q", problem.Code)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "Idempotency-Key" {
		t.Errorf("errors = %+v", problem.Errors)
	}
}

func TestNilErrorBecomesInternalError(t *testing.T) {
	problem := apierror.From(nil)

	if problem.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", problem.Status)
	}
}

func TestReadyMadeConstructors(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{apierror.BadRequest("x"), http.StatusBadRequest, apierror.CodeBadRequest},
		{apierror.Unauthorized("x"), http.StatusUnauthorized, apierror.CodeUnauthorized},
		{apierror.MethodNotAllowed("PUT"), http.StatusMethodNotAllowed, apierror.CodeMethodNotAllowed},
		{apierror.NotFoundEndpoint("/x"), http.StatusNotFound, apierror.CodeNotFound},
		{apierror.RateLimited(), http.StatusTooManyRequests, apierror.CodeRateLimited},
		{apierror.Unavailable("x"), http.StatusServiceUnavailable, apierror.CodeUnavailable},
		{apierror.InternalPanic(), http.StatusInternalServerError, apierror.CodeInternal},
	}

	for _, tc := range cases {
		problem := apierror.From(tc.err)
		if problem.Status != tc.wantStatus || problem.Code != tc.wantCode {
			t.Errorf("%v -> %d/%s, want %d/%s", tc.err, problem.Status, problem.Code, tc.wantStatus, tc.wantCode)
		}
	}
}
