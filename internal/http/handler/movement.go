package handler

import (
	"log/slog"
	"net/http"

	"github.com/example/wallet-service/internal/usecase"
	"github.com/example/wallet-service/pkg/apierror"
	"github.com/example/wallet-service/pkg/response"
)

// MovementHandler serves the money movement endpoints (top-up, withdraw, transfer).
type MovementHandler struct {
	service      MovementService
	metrics      MetricsRecorder
	maxBodyBytes int64
	log          *slog.Logger
}

func NewMovementHandler(service MovementService, metrics MetricsRecorder, maxBodyBytes int64, log *slog.Logger) *MovementHandler {
	return &MovementHandler{service: service, metrics: metrics, maxBodyBytes: maxBodyBytes, log: log}
}

type moneyMovementRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Reference   string `json:"reference"`
}

type transferRequest struct {
	ToWalletID  string `json:"to_wallet_id"`
	AmountMinor int64  `json:"amount_minor"`
	Reference   string `json:"reference"`
}

// TopUp handles POST /v1/wallets/{walletID}/topup.
func (h *MovementHandler) TopUp(w http.ResponseWriter, r *http.Request) {
	key, err := requireIdempotencyKey(r)
	if err != nil {
		response.Error(w, r, err)
		return
	}

	var body moneyMovementRequest
	if err := decodeJSON(w, r, &body, h.maxBodyBytes); err != nil {
		response.Error(w, r, err)
		return
	}

	view, err := h.service.TopUp(r.Context(), usecase.TopUpInput{
		WalletID:       r.PathValue("walletID"),
		AmountMinor:    body.AmountMinor,
		Reference:      body.Reference,
		IdempotencyKey: key,
	})
	if err != nil {
		recordTransaction(h.metrics, "topup", apierror.From(err).Code)
		response.Error(w, r, err)
		return
	}
	h.writeMovement(w, r, "topup", view)
}

// Withdraw handles POST /v1/wallets/{walletID}/withdraw.
func (h *MovementHandler) Withdraw(w http.ResponseWriter, r *http.Request) {
	key, err := requireIdempotencyKey(r)
	if err != nil {
		response.Error(w, r, err)
		return
	}

	var body moneyMovementRequest
	if err := decodeJSON(w, r, &body, h.maxBodyBytes); err != nil {
		response.Error(w, r, err)
		return
	}

	view, err := h.service.Withdraw(r.Context(), usecase.WithdrawInput{
		WalletID:       r.PathValue("walletID"),
		AmountMinor:    body.AmountMinor,
		Reference:      body.Reference,
		IdempotencyKey: key,
	})
	if err != nil {
		recordTransaction(h.metrics, "withdrawal", apierror.From(err).Code)
		response.Error(w, r, err)
		return
	}
	h.writeMovement(w, r, "withdrawal", view)
}

// Transfer handles POST /v1/wallets/{walletID}/transfer.
func (h *MovementHandler) Transfer(w http.ResponseWriter, r *http.Request) {
	key, err := requireIdempotencyKey(r)
	if err != nil {
		response.Error(w, r, err)
		return
	}

	var body transferRequest
	if err := decodeJSON(w, r, &body, h.maxBodyBytes); err != nil {
		response.Error(w, r, err)
		return
	}

	view, err := h.service.Transfer(r.Context(), usecase.TransferInput{
		FromWalletID:   r.PathValue("walletID"),
		ToWalletID:     body.ToWalletID,
		AmountMinor:    body.AmountMinor,
		Reference:      body.Reference,
		IdempotencyKey: key,
	})
	if err != nil {
		recordTransaction(h.metrics, "transfer", apierror.From(err).Code)
		response.Error(w, r, err)
		return
	}
	h.writeMovement(w, r, "transfer", view)
}

func (h *MovementHandler) writeMovement(w http.ResponseWriter, r *http.Request, kind string, view *usecase.MovementView) {
	result := "success"
	if view.Replayed {
		result = "replayed"
		w.Header().Set(IdempotencyReplayedHeader, "true")
	}
	recordTransaction(h.metrics, kind, result)
	response.JSON(w, http.StatusOK, view)
}

// requireIdempotencyKey ensures the header is present.
//
// A missing header is a malformed request (400). Unreasonable key formats
// (too short or containing odd characters) are validated in the domain layer
// as semantic errors (422).
func requireIdempotencyKey(r *http.Request) (string, error) {
	key := r.Header.Get(IdempotencyKeyHeader)
	if key == "" {
		return "", apierror.BadRequestFields(
			"header "+IdempotencyKeyHeader+" is required for requests that change data",
			apierror.FieldError{Field: IdempotencyKeyHeader, Message: "header is required"},
		)
	}
	return key, nil
}
