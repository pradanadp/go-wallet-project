package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/wallet-service/internal/domain"
	"github.com/example/wallet-service/internal/http/handler"
	"github.com/example/wallet-service/internal/http/middleware"
	"github.com/example/wallet-service/internal/usecase"
)

const (
	walletID      = "11111111-1111-4111-8111-111111111111"
	otherWallet   = "44444444-4444-4444-8444-444444444444"
	transactionID = "33333333-3333-4333-8333-333333333333"
)

// errUnexpected simulates an unrecognized internal failure (e.g. a bug).
var errUnexpected = errors.New("boom: unexpected internal failure")

// Manual fakes (no mocking framework): structs with per-test configurable
// behaviour. Enough because the interfaces are small and stable.

type stubWalletService struct {
	createResult *usecase.WalletView
	createErr    error
	lastCreate   usecase.CreateWalletInput

	getResult *usecase.WalletView
	getErr    error
	lastGet   usecase.GetWalletInput

	listResult *usecase.TransactionPage
	listErr    error
	lastList   usecase.ListTransactionsInput
}

func (s *stubWalletService) CreateWallet(_ context.Context, in usecase.CreateWalletInput) (*usecase.WalletView, error) {
	s.lastCreate = in
	return s.createResult, s.createErr
}

func (s *stubWalletService) GetWallet(_ context.Context, in usecase.GetWalletInput) (*usecase.WalletView, error) {
	s.lastGet = in
	return s.getResult, s.getErr
}

func (s *stubWalletService) ListTransactions(_ context.Context, in usecase.ListTransactionsInput) (*usecase.TransactionPage, error) {
	s.lastList = in
	return s.listResult, s.listErr
}

type stubMovementService struct {
	topUpResult *usecase.MovementView
	topUpErr    error
	lastTopUp   usecase.TopUpInput

	withdrawResult *usecase.MovementView
	withdrawErr    error
	lastWithdraw   usecase.WithdrawInput

	transferResult *usecase.MovementView
	transferErr    error
	lastTransfer   usecase.TransferInput
}

func (s *stubMovementService) TopUp(_ context.Context, in usecase.TopUpInput) (*usecase.MovementView, error) {
	s.lastTopUp = in
	return s.topUpResult, s.topUpErr
}

func (s *stubMovementService) Withdraw(_ context.Context, in usecase.WithdrawInput) (*usecase.MovementView, error) {
	s.lastWithdraw = in
	return s.withdrawResult, s.withdrawErr
}

func (s *stubMovementService) Transfer(_ context.Context, in usecase.TransferInput) (*usecase.MovementView, error) {
	s.lastTransfer = in
	return s.transferResult, s.transferErr
}

type metricsSpy struct{ calls []string }

func (m *metricsSpy) RecordTransaction(kind, result string) {
	m.calls = append(m.calls, kind+"="+result)
}

type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

type testServer struct {
	router    http.Handler
	wallets   *stubWalletService
	movements *stubMovementService
	metrics   *metricsSpy
}

func newTestServer() *testServer {
	wallets := &stubWalletService{}
	movements := &stubMovementService{}
	spy := &metricsSpy{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	router := handler.NewRouter(
		handler.Dependencies{
			Wallets:   handler.NewWalletHandler(wallets, spy, 1<<20, log),
			Movements: handler.NewMovementHandler(movements, spy, 1<<20, log),
			Health:    handler.NewHealthHandler(stubPinger{}, "test"),
		},
		middleware.RequestID(),
	)

	return &testServer{router: router, wallets: wallets, movements: movements, metrics: spy}
}

type requestOptions struct {
	body           string
	idempotencyKey string
}

func (s *testServer) do(t *testing.T, method, path string, opts requestOptions) *httptest.ResponseRecorder {
	t.Helper()

	var body io.Reader
	if opts.body != "" {
		body = strings.NewReader(opts.body)
	}
	request := httptest.NewRequest(method, path, body)
	if opts.body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if opts.idempotencyKey != "" {
		request.Header.Set(handler.IdempotencyKeyHeader, opts.idempotencyKey)
	}

	recorder := httptest.NewRecorder()
	s.router.ServeHTTP(recorder, request)
	return recorder
}

func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var problem map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not a JSON problem: %v (%s)", err, recorder.Body.String())
	}
	return problem
}

func TestCreateWalletReturns201AndLocation(t *testing.T) {
	server := newTestServer()
	server.wallets.createResult = &usecase.WalletView{
		ID: "11111111-1111-4111-8111-111111111111", OwnerID: "22222222-2222-4222-8222-222222222222",
		Currency: "IDR", Status: "active", BalanceMinor: 0,
	}

	recorder := server.do(t, http.MethodPost, "/v1/wallets", requestOptions{
		body: `{"owner_id":"22222222-2222-4222-8222-222222222222","currency":"IDR"}`,
	})

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "/v1/wallets/11111111-1111-4111-8111-111111111111" {
		t.Errorf("Location = %q", location)
	}
	if server.wallets.lastCreate.OwnerID != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("owner_id input = %q", server.wallets.lastCreate.OwnerID)
	}

	var view usecase.WalletView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if view.ID != server.wallets.createResult.ID {
		t.Errorf("response id = %q", view.ID)
	}
}

func TestCreateWalletRejectsUnknownAndMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `{owner_id}`},
		{name: "unknown field", body: `{"owner_id":"22222222-2222-4222-8222-222222222222","currency":"IDR","limit":10}`},
		{name: "wrong type", body: `{"owner_id":123,"currency":"IDR"}`},
		{name: "empty body", body: " "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer()

			recorder := server.do(t, http.MethodPost, "/v1/wallets", requestOptions{body: tc.body})

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
			}
			if code := decodeProblem(t, recorder)["code"]; code != "bad_request" {
				t.Errorf("code = %v, want bad_request", code)
			}
		})
	}
}

func TestGetWalletMapsNotFoundToRFC7807(t *testing.T) {
	server := newTestServer()
	server.wallets.getErr = &domain.NotFoundError{Resource: "wallet", ID: "11111111-1111-4111-8111-111111111111"}

	recorder := server.do(t, http.MethodGet, "/v1/wallets/11111111-1111-4111-8111-111111111111", requestOptions{})

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	problem := decodeProblem(t, recorder)
	if problem["code"] != "not_found" {
		t.Errorf("code = %v, want not_found", problem["code"])
	}
	if problem["instance"] != "/v1/wallets/11111111-1111-4111-8111-111111111111" {
		t.Errorf("instance = %v", problem["instance"])
	}
	if problem["request_id"] == "" {
		t.Error("request_id is empty in the problem details")
	}
	if server.wallets.lastGet.WalletID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("walletID from the path = %q", server.wallets.lastGet.WalletID)
	}
}

func TestTopUpRequiresIdempotencyKeyHeader(t *testing.T) {
	server := newTestServer()

	recorder := server.do(t, http.MethodPost, "/v1/wallets/11111111-1111-4111-8111-111111111111/topup",
		requestOptions{body: `{"amount_minor":1000}`})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	problem := decodeProblem(t, recorder)
	fields, ok := problem["errors"].([]any)
	if !ok || len(fields) == 0 {
		t.Fatalf("errors[] is missing from the problem: %v", problem)
	}
}

func TestTopUpSuccessAndReplayHeaders(t *testing.T) {
	server := newTestServer()
	server.movements.topUpResult = &usecase.MovementView{
		TransactionID: "33333333-3333-4333-8333-333333333333",
		Type:          "topup", Status: "posted",
		WalletID:    "11111111-1111-4111-8111-111111111111",
		AmountMinor: 5_000, Currency: "IDR", BalanceAfter: 5_000,
	}

	first := server.do(t, http.MethodPost, "/v1/wallets/11111111-1111-4111-8111-111111111111/topup",
		requestOptions{body: `{"amount_minor":5000,"reference":"va-123"}`, idempotencyKey: "topup-key-0001"})

	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", first.Code, first.Body.String())
	}
	if first.Header().Get(handler.IdempotencyReplayedHeader) != "" {
		t.Error("the first request must not be marked as replayed")
	}
	if got := server.metrics.calls; len(got) != 1 || got[0] != "topup=success" {
		t.Errorf("metrics = %v, want [topup=success]", got)
	}
	if server.movements.lastTopUp.AmountMinor != 5_000 ||
		server.movements.lastTopUp.Reference != "va-123" ||
		server.movements.lastTopUp.IdempotencyKey != "topup-key-0001" ||
		server.movements.lastTopUp.WalletID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("input topup = %+v", server.movements.lastTopUp)
	}

	server.movements.topUpResult.Replayed = true
	second := server.do(t, http.MethodPost, "/v1/wallets/11111111-1111-4111-8111-111111111111/topup",
		requestOptions{body: `{"amount_minor":5000,"reference":"va-123"}`, idempotencyKey: "topup-key-0001"})

	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", second.Code)
	}
	if second.Header().Get(handler.IdempotencyReplayedHeader) != "true" {
		t.Error("the Idempotency-Replayed header was not set on the replay")
	}
	if got := server.metrics.calls; len(got) != 2 || got[1] != "topup=replayed" {
		t.Errorf("metrics = %v, want the second entry to be topup=replayed", got)
	}
}

func TestTopUpMapsDomainErrorsToStatusCodes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "insufficient balance",
			err: &domain.InsufficientFundsError{
				WalletID: walletID, Balance: 1_000, Required: 5_000,
			},
			wantStatus: http.StatusConflict,
			wantCode:   "insufficient_funds",
		},
		{
			name:       "wallet not active",
			err:        &domain.WalletNotActiveError{WalletID: walletID, Status: domain.WalletStatusFrozen},
			wantStatus: http.StatusConflict,
			wantCode:   "wallet_not_active",
		},
		{
			name:       "idempotency key reused with a different payload",
			err:        &domain.IdempotencyConflictError{Key: "topup-key-0001"},
			wantStatus: http.StatusConflict,
			wantCode:   "idempotency_conflict",
		},
		{
			name:       "amount validation",
			err:        domain.NewValidationError("amount_minor", "must be greater than 0"),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_error",
		},
		{
			name:       "unexpected error",
			err:        errUnexpected,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer()
			server.movements.topUpErr = tc.err

			recorder := server.do(t, http.MethodPost, "/v1/wallets/11111111-1111-4111-8111-111111111111/topup",
				requestOptions{body: `{"amount_minor":1000}`, idempotencyKey: "topup-key-0001"})

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			problem := decodeProblem(t, recorder)
			if problem["code"] != tc.wantCode {
				t.Errorf("code = %v, want %s", problem["code"], tc.wantCode)
			}
			if got := server.metrics.calls; len(got) != 1 || got[0] != "topup="+tc.wantCode {
				t.Errorf("metrics = %v, want topup=%s", got, tc.wantCode)
			}
		})
	}
}

func TestTransferRejectsSelfTransferWith422(t *testing.T) {
	server := newTestServer()
	server.movements.transferErr = &domain.SelfTransferError{WalletID: walletID}

	recorder := server.do(t, http.MethodPost, "/v1/wallets/11111111-1111-4111-8111-111111111111/transfer", requestOptions{
		body:           `{"to_wallet_id":"11111111-1111-4111-8111-111111111111","amount_minor":1000}`,
		idempotencyKey: "transfer-key-0001",
	})

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", recorder.Code)
	}
	if code := decodeProblem(t, recorder)["code"]; code != "self_transfer" {
		t.Errorf("code = %v, want self_transfer", code)
	}
}

func TestListTransactionsPaginationMetadata(t *testing.T) {
	server := newTestServer()
	server.wallets.listResult = &usecase.TransactionPage{
		Records: []usecase.LedgerRecordView{
			{EntryID: 7, TransactionID: "33333333-3333-4333-8333-333333333333", Type: "topup", Direction: "credit", AmountMinor: 1_000},
		},
		NextEntryID: 7,
		HasMore:     true,
	}

	recorder := server.do(t, http.MethodGet, "/v1/wallets/11111111-1111-4111-8111-111111111111/transactions?limit=1", requestOptions{})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var page struct {
		Data       []usecase.LedgerRecordView `json:"data"`
		Pagination struct {
			NextCursor string `json:"next_cursor"`
			HasMore    bool   `json:"has_more"`
			Limit      int    `json:"limit"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].EntryID != 7 {
		t.Errorf("data = %+v", page.Data)
	}
	if !page.Pagination.HasMore || page.Pagination.Limit != 1 {
		t.Errorf("pagination = %+v", page.Pagination)
	}
	if page.Pagination.NextCursor == "" {
		t.Error("next_cursor is empty even though has_more=true")
	}
	if server.wallets.lastList.Limit != 1 {
		t.Errorf("limit diteruskan = %d, ingin 1", server.wallets.lastList.Limit)
	}
}

func TestListTransactionsDecodesCursor(t *testing.T) {
	server := newTestServer()
	server.wallets.listResult = &usecase.TransactionPage{}

	// cursor from the previous response: base64url("e:42")
	recorder := server.do(t, http.MethodGet,
		"/v1/wallets/11111111-1111-4111-8111-111111111111/transactions?cursor=ZTo0Mg", requestOptions{})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, ingin 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if server.wallets.lastList.AfterEntryID != 42 {
		t.Errorf("afterEntryID = %d, want 42", server.wallets.lastList.AfterEntryID)
	}
}

func TestListTransactionsRejectsBadQueryParameters(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantCode   string
	}{
		{name: "invalid cursor", query: "?cursor=!!!", wantStatus: http.StatusBadRequest, wantCode: "bad_request"},
		{name: "limit is not a number", query: "?limit=many", wantStatus: http.StatusBadRequest, wantCode: "bad_request"},
		{name: "limit out of range", query: "?limit=1000", wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_error"},
		{name: "negative cursor", query: "?cursor=ZTotMQ", wantStatus: http.StatusBadRequest, wantCode: "bad_request"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer()
			if tc.wantStatus == http.StatusUnprocessableEntity {
				server.wallets.listErr = domain.NewValidationError("limit", "must be between 1 and 100")
			}

			recorder := server.do(t, http.MethodGet,
				"/v1/wallets/11111111-1111-4111-8111-111111111111/transactions"+tc.query, requestOptions{})

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if code := decodeProblem(t, recorder)["code"]; code != tc.wantCode {
				t.Errorf("code = %v, want %s", code, tc.wantCode)
			}
		})
	}
}

func TestHealthEndpointsUsePinger(t *testing.T) {
	server := newTestServer()

	live := server.do(t, http.MethodGet, "/healthz", requestOptions{})
	if live.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", live.Code)
	}

	ready := server.do(t, http.MethodGet, "/readyz", requestOptions{})
	if ready.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", ready.Code)
	}

	unhealthy := newTestServer()
	unhealthy.router = handler.NewRouter(handler.Dependencies{
		Wallets:   handler.NewWalletHandler(&stubWalletService{}, nil, 1<<20, nil),
		Movements: handler.NewMovementHandler(&stubMovementService{}, nil, 1<<20, nil),
		Health:    handler.NewHealthHandler(stubPinger{err: errUnexpected}, "test"),
	})
	recorder := unhealthy.do(t, http.MethodGet, "/readyz", requestOptions{})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", recorder.Code)
	}
	if code := decodeProblem(t, recorder)["code"]; code != "database_unavailable" {
		t.Errorf("code = %v, want database_unavailable", code)
	}
}
