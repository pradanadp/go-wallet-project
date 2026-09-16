package handler

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/example/wallet-service/internal/usecase"
	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/cursor"
	"github.com/example/wallet-service/pkg/response"
)

// IdempotencyKeyHeader is the required header for money movement endpoints.
const IdempotencyKeyHeader = "Idempotency-Key"

// IdempotencyReplayedHeader marks a response that replays a previous identical
// request (no new business effect happened).
const IdempotencyReplayedHeader = "Idempotency-Replayed"

// WalletHandler serves the /v1/wallets endpoints.
type WalletHandler struct {
	service      WalletService
	metrics      MetricsRecorder
	maxBodyBytes int64
	log          *slog.Logger
}

func NewWalletHandler(service WalletService, metrics MetricsRecorder, maxBodyBytes int64, log *slog.Logger) *WalletHandler {
	return &WalletHandler{service: service, metrics: metrics, maxBodyBytes: maxBodyBytes, log: log}
}

type createWalletRequest struct {
	OwnerID  string `json:"owner_id"`
	Currency string `json:"currency"`
}

// Create handles POST /v1/wallets.
func (h *WalletHandler) Create(w http.ResponseWriter, r *http.Request) {
	var body createWalletRequest
	if err := decodeJSON(w, r, &body, h.maxBodyBytes); err != nil {
		response.Error(w, r, err)
		return
	}

	view, err := h.service.CreateWallet(r.Context(), usecase.CreateWalletInput{
		OwnerID:        body.OwnerID,
		Currency:       body.Currency,
		IdempotencyKey: r.Header.Get(IdempotencyKeyHeader),
	})
	if err != nil {
		response.Error(w, r, err)
		return
	}

	if view.Replayed {
		w.Header().Set(IdempotencyReplayedHeader, "true")
	}
	w.Header().Set("Location", "/v1/wallets/"+view.ID)
	response.JSON(w, http.StatusCreated, view)
}

// Get handles GET /v1/wallets/{walletID}.
func (h *WalletHandler) Get(w http.ResponseWriter, r *http.Request) {
	view, err := h.service.GetWallet(r.Context(), usecase.GetWalletInput{
		WalletID: r.PathValue("walletID"),
	})
	if err != nil {
		response.Error(w, r, err)
		return
	}
	response.JSON(w, http.StatusOK, view)
}

// ListTransactions handles GET /v1/wallets/{walletID}/transactions.
func (h *WalletHandler) ListTransactions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	limit, err := parseLimit(query.Get("limit"))
	if err != nil {
		response.Error(w, r, err)
		return
	}

	var afterEntryID int64
	if raw := query.Get("cursor"); raw != "" {
		afterEntryID, err = cursor.DecodeEntry(raw)
		if err != nil {
			response.Error(w, r, err)
			return
		}
	}

	page, err := h.service.ListTransactions(r.Context(), usecase.ListTransactionsInput{
		WalletID:     r.PathValue("walletID"),
		AfterEntryID: afterEntryID,
		Limit:        limit,
	})
	if err != nil {
		response.Error(w, r, err)
		return
	}

	result := response.Page[usecase.LedgerRecordView]{
		Data: page.Records,
		Pagination: response.Pagination{
			HasMore: page.HasMore,
			Limit:   limit,
		},
	}
	if page.HasMore {
		result.Pagination.NextCursor = cursor.EncodeEntry(page.NextEntryID)
	}
	response.JSON(w, http.StatusOK, result)
}

// parseLimit reads the limit parameter. A non-numeric value is a malformed
// request (400), whereas an out-of-range value is validated by the usecase and
// becomes 422.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return usecase.DefaultPageLimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, apierror.BadRequestFields("invalid limit parameter",
			apierror.FieldError{Field: "limit", Message: "must be an integer"})
	}
	return value, nil
}
