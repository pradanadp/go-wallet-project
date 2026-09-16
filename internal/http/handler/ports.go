// Package handler contains the HTTP handlers. Their only job is to translate HTTP
// (path, query, header, body) into usecase calls and back. No business rules live
// in this layer.
package handler

import (
	"context"

	"github.com/example/wallet-service/internal/usecase"
)

// WalletService is the port the wallet handler needs. It is declared on the
// consumer side (not in the usecase) so the handler can be tested with a small
// fake without touching the real usecase implementation.
type WalletService interface {
	CreateWallet(ctx context.Context, in usecase.CreateWalletInput) (*usecase.WalletView, error)
	GetWallet(ctx context.Context, in usecase.GetWalletInput) (*usecase.WalletView, error)
	ListTransactions(ctx context.Context, in usecase.ListTransactionsInput) (*usecase.TransactionPage, error)
}

// MovementService is the port the money movement handler needs.
type MovementService interface {
	TopUp(ctx context.Context, in usecase.TopUpInput) (*usecase.MovementView, error)
	Withdraw(ctx context.Context, in usecase.WithdrawInput) (*usecase.MovementView, error)
	Transfer(ctx context.Context, in usecase.TransferInput) (*usecase.MovementView, error)
}

// MetricsRecorder records money movement outcomes for Prometheus metrics.
// This small interface keeps the handler independent from the metrics library.
type MetricsRecorder interface {
	RecordTransaction(kind, result string)
}

// Pinger is the dependency used by the readiness probe (implemented by *pgxpool.Pool).
type Pinger interface {
	Ping(ctx context.Context) error
}

// recordTransaction is safe to call even when no recorder is wired (e.g. in tests).
func recordTransaction(recorder MetricsRecorder, kind, result string) {
	if recorder == nil {
		return
	}
	recorder.RecordTransaction(kind, result)
}
