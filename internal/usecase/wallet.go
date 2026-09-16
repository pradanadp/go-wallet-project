package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/example/wallet-service/internal/domain"
)

// WalletUseCase handles operations that do not mutate balances: wallet creation,
// wallet lookups, and transaction history.
type WalletUseCase struct {
	uow            UnitOfWork
	clock          Clock
	newID          IDGenerator
	idempotencyTTL time.Duration
}

func NewWalletUseCase(uow UnitOfWork, clock Clock, newID IDGenerator, idempotencyTTL time.Duration) *WalletUseCase {
	return &WalletUseCase{uow: uow, clock: clock, newID: newID, idempotencyTTL: idempotencyTTL}
}

// CreateWalletInput is the wallet creation input.
type CreateWalletInput struct {
	OwnerID        string
	Currency       string
	IdempotencyKey string
}

// GetWalletInput is the wallet lookup input.
type GetWalletInput struct {
	WalletID string
}

// CreateWallet creates a new wallet for a user.
//
// Idempotency-Key is optional here: when provided, a double submit will not
// create two wallets.
func (uc *WalletUseCase) CreateWallet(ctx context.Context, in CreateWalletInput) (*WalletView, error) {
	ownerID, err := domain.ParseID("owner_id", in.OwnerID)
	if err != nil {
		return nil, err
	}
	currency, err := domain.ParseCurrency("currency", in.Currency)
	if err != nil {
		return nil, err
	}

	spec := idempotentSpec{
		key:         in.IdempotencyKey,
		endpoint:    endpointCreateWallet,
		requestHash: hashRequest(endpointCreateWallet, ownerID.String(), currency.String()),
		statusCode:  statusCreated,
		optionalKey: true,
	}

	view, replayed, err := runIdempotent(ctx, uc.uow, uc.clock, uc.idempotencyTTL, spec,
		func(ctx context.Context, repos Repos) (*WalletView, string, error) {
			return uc.createWallet(ctx, repos, ownerID, currency)
		})
	if err != nil {
		return nil, err
	}
	view.Replayed = replayed
	return view, nil
}

func (uc *WalletUseCase) createWallet(ctx context.Context, repos Repos, ownerID domain.ID, currency domain.Currency) (*WalletView, string, error) {
	rawID, err := uc.newID()
	if err != nil {
		return nil, "", fmt.Errorf("generate wallet id: %w", err)
	}
	walletID, err := domain.ParseID("wallet_id", rawID)
	if err != nil {
		return nil, "", fmt.Errorf("generate wallet id: %w", err)
	}

	now := uc.clock()
	w := domain.NewWallet(ownerID, currency, now)
	w.ID = walletID

	if err := repos.Wallets().Create(ctx, w); err != nil {
		return nil, "", err
	}

	view := walletToView(w)
	return &view, w.ID.String(), nil
}

// GetWallet returns the wallet details including its balance.
func (uc *WalletUseCase) GetWallet(ctx context.Context, in GetWalletInput) (*WalletView, error) {
	walletID, err := domain.ParseID("wallet_id", in.WalletID)
	if err != nil {
		return nil, err
	}

	w, err := uc.uow.ReadRepos().Wallets().GetByID(ctx, walletID)
	if err != nil {
		return nil, err
	}
	view := walletToView(w)
	return &view, nil
}

// ListTransactionsInput is the transaction history input (cursor pagination).
type ListTransactionsInput struct {
	WalletID string
	// AfterEntryID is the cursor position: 0 means start from the newest transaction.
	AfterEntryID int64
	// Limit 0 means DefaultPageLimit is used.
	Limit int
}

// ListTransactions returns one wallet's ledger history, newest first.
//
// The cursor uses the monotonic entry id (not OFFSET), so a page never skips or
// duplicates rows when new transactions arrive between two requests.
func (uc *WalletUseCase) ListTransactions(ctx context.Context, in ListTransactionsInput) (*TransactionPage, error) {
	walletID, err := domain.ParseID("wallet_id", in.WalletID)
	if err != nil {
		return nil, err
	}
	if in.AfterEntryID < 0 {
		return nil, domain.NewValidationError("cursor", "invalid cursor")
	}
	limit := in.Limit
	if limit == 0 {
		limit = DefaultPageLimit
	}
	if limit < 1 || limit > MaxPageLimit {
		return nil, domain.NewValidationError("limit", fmt.Sprintf("must be between 1 and %d", MaxPageLimit))
	}

	repos := uc.uow.ReadRepos()
	// The wallet must exist: a wallet without transactions still returns 200 with
	// empty data, whereas an unknown wallet must return 404.
	if _, err := repos.Wallets().GetByID(ctx, walletID); err != nil {
		return nil, err
	}

	// Fetch limit+1 to detect another page without COUNT(*).
	records, err := repos.Ledger().ListRecordsByWallet(ctx, walletID, in.AfterEntryID, limit+1)
	if err != nil {
		return nil, err
	}

	page := &TransactionPage{Records: make([]LedgerRecordView, 0, limit)}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	for _, rec := range records {
		page.Records = append(page.Records, ledgerRecordToView(rec))
	}
	if hasMore && len(records) > 0 {
		page.NextEntryID = records[len(records)-1].EntryID
	}
	page.HasMore = hasMore
	return page, nil
}

func walletToView(w *domain.Wallet) WalletView {
	return WalletView{
		ID:           w.ID.String(),
		OwnerID:      w.OwnerID.String(),
		Currency:     w.Currency.String(),
		Status:       string(w.Status),
		BalanceMinor: w.Balance.Int64(),
		CreatedAt:    w.CreatedAt.UTC(),
		UpdatedAt:    w.UpdatedAt.UTC(),
	}
}

func ledgerRecordToView(rec domain.LedgerRecord) LedgerRecordView {
	view := LedgerRecordView{
		EntryID:           rec.EntryID,
		TransactionID:     rec.TransactionID.String(),
		Type:              string(rec.Type),
		Direction:         string(rec.Direction),
		AmountMinor:       rec.Amount.Int64(),
		BalanceAfterMinor: rec.BalanceAfter.Int64(),
		Currency:          rec.Currency.String(),
		Reference:         rec.Reference,
		CreatedAt:         rec.CreatedAt.UTC(),
	}
	if rec.CounterpartyWalletID != nil {
		view.CounterpartyWalletID = rec.CounterpartyWalletID.String()
	}
	return view
}
