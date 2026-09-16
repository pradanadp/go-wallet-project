package usecase

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/example/wallet-service/internal/domain"
)

// Endpoint names are part of the idempotency fingerprint, so the same key on a
// different endpoint is always treated as a conflict.
const (
	endpointCreateWallet = "POST /v1/wallets"
	endpointTopUp        = "POST /v1/wallets/{id}/topup"
	endpointWithdraw     = "POST /v1/wallets/{id}/withdraw"
	endpointTransfer     = "POST /v1/wallets/{id}/transfer"
)

const (
	statusCreated = 201
)

// MovementUseCase handles every money movement: top-up, withdraw, transfer.
// Each operation writes one double-entry journal and updates wallet balances
// atomically inside a single database transaction.
type MovementUseCase struct {
	uow            UnitOfWork
	clock          Clock
	newID          IDGenerator
	systemWalletID domain.ID
	idempotencyTTL time.Duration
}

func NewMovementUseCase(
	uow UnitOfWork,
	clock Clock,
	newID IDGenerator,
	systemWalletID domain.ID,
	idempotencyTTL time.Duration,
) *MovementUseCase {
	return &MovementUseCase{
		uow:            uow,
		clock:          clock,
		newID:          newID,
		systemWalletID: systemWalletID,
		idempotencyTTL: idempotencyTTL,
	}
}

// TopUpInput is the top-up input.
type TopUpInput struct {
	WalletID       string
	AmountMinor    int64
	Reference      string
	IdempotencyKey string
}

// WithdrawInput is the withdrawal input.
type WithdrawInput struct {
	WalletID       string
	AmountMinor    int64
	Reference      string
	IdempotencyKey string
}

// TransferInput is the wallet-to-wallet transfer input.
type TransferInput struct {
	FromWalletID   string
	ToWalletID     string
	AmountMinor    int64
	Reference      string
	IdempotencyKey string
}

// TopUp adds balance to a wallet from an external funding source.
//
//	DEBIT  system wallet (world account)
//	CREDIT user wallet
func (uc *MovementUseCase) TopUp(ctx context.Context, in TopUpInput) (*MovementView, error) {
	walletID, amount, reference, err := parseMovementInput(in.WalletID, in.AmountMinor, in.Reference)
	if err != nil {
		return nil, err
	}

	spec := idempotentSpec{
		key:         in.IdempotencyKey,
		endpoint:    endpointTopUp,
		requestHash: hashRequest(endpointTopUp, walletID.String(), amount.String(), reference),
		statusCode:  statusOK,
	}

	view, replayed, err := runIdempotent(ctx, uc.uow, uc.clock, uc.idempotencyTTL, spec,
		func(ctx context.Context, repos Repos) (*MovementView, string, error) {
			return uc.postTopUp(ctx, repos, walletID, amount, reference, in.IdempotencyKey)
		})
	if err != nil {
		return nil, err
	}
	view.Replayed = replayed
	return view, nil
}

func (uc *MovementUseCase) postTopUp(ctx context.Context, repos Repos, walletID domain.ID, amount domain.Amount, reference, idempotencyKey string) (*MovementView, string, error) {
	locked, err := uc.lockWallets(ctx, repos, walletID, uc.systemWalletID)
	if err != nil {
		return nil, "", err
	}
	user, world := locked[walletID], locked[uc.systemWalletID]

	postings, err := domain.PostTopUp(world, user, amount)
	if err != nil {
		return nil, "", err
	}
	if err := domain.AssertBalanced(postings); err != nil {
		return nil, "", err
	}

	txn, err := uc.commitMovement(ctx, repos, domain.TransactionTypeTopUp, reference, idempotencyKey,
		map[string]any{"system_wallet_id": world.ID.String()}, postings, []*domain.Wallet{world, user})
	if err != nil {
		return nil, "", err
	}

	view := movementToView(txn, user, amount, "")
	return &view, txn.ID.String(), nil
}

// Withdraw takes balance out of the system; rejected when funds are insufficient.
//
//	DEBIT  user wallet
//	CREDIT system wallet (world account)
func (uc *MovementUseCase) Withdraw(ctx context.Context, in WithdrawInput) (*MovementView, error) {
	walletID, amount, reference, err := parseMovementInput(in.WalletID, in.AmountMinor, in.Reference)
	if err != nil {
		return nil, err
	}

	spec := idempotentSpec{
		key:         in.IdempotencyKey,
		endpoint:    endpointWithdraw,
		requestHash: hashRequest(endpointWithdraw, walletID.String(), amount.String(), reference),
		statusCode:  statusOK,
	}

	view, replayed, err := runIdempotent(ctx, uc.uow, uc.clock, uc.idempotencyTTL, spec,
		func(ctx context.Context, repos Repos) (*MovementView, string, error) {
			return uc.postWithdrawal(ctx, repos, walletID, amount, reference, in.IdempotencyKey)
		})
	if err != nil {
		return nil, err
	}
	view.Replayed = replayed
	return view, nil
}

func (uc *MovementUseCase) postWithdrawal(ctx context.Context, repos Repos, walletID domain.ID, amount domain.Amount, reference, idempotencyKey string) (*MovementView, string, error) {
	locked, err := uc.lockWallets(ctx, repos, walletID, uc.systemWalletID)
	if err != nil {
		return nil, "", err
	}
	user, world := locked[walletID], locked[uc.systemWalletID]

	postings, err := domain.PostWithdrawal(world, user, amount)
	if err != nil {
		return nil, "", err
	}
	if err := domain.AssertBalanced(postings); err != nil {
		return nil, "", err
	}

	txn, err := uc.commitMovement(ctx, repos, domain.TransactionTypeWithdrawal, reference, idempotencyKey,
		map[string]any{"system_wallet_id": world.ID.String()}, postings, []*domain.Wallet{world, user})
	if err != nil {
		return nil, "", err
	}

	view := movementToView(txn, user, amount, "")
	return &view, txn.ID.String(), nil
}

// Transfer moves balance between two user wallets atomically:
//
//	DEBIT  source wallet
//	CREDIT destination wallet
//
// Both wallets are locked (SELECT ... FOR UPDATE) in sorted ID order, so an
// A->B transfer can never deadlock with a B->A transfer and the source balance
// cannot be spent twice concurrently.
func (uc *MovementUseCase) Transfer(ctx context.Context, in TransferInput) (*MovementView, error) {
	fromID, err := domain.ParseID("wallet_id", in.FromWalletID)
	if err != nil {
		return nil, err
	}
	toID, err := domain.ParseID("to_wallet_id", in.ToWalletID)
	if err != nil {
		return nil, err
	}
	if fromID == toID {
		return nil, &domain.SelfTransferError{WalletID: fromID}
	}
	amount, err := domain.NewAmount("amount_minor", in.AmountMinor)
	if err != nil {
		return nil, err
	}
	reference, err := normalizeReference(in.Reference)
	if err != nil {
		return nil, err
	}

	spec := idempotentSpec{
		key:         in.IdempotencyKey,
		endpoint:    endpointTransfer,
		requestHash: hashRequest(endpointTransfer, fromID.String(), toID.String(), amount.String(), reference),
		statusCode:  statusOK,
	}

	view, replayed, err := runIdempotent(ctx, uc.uow, uc.clock, uc.idempotencyTTL, spec,
		func(ctx context.Context, repos Repos) (*MovementView, string, error) {
			return uc.postTransfer(ctx, repos, fromID, toID, amount, reference, in.IdempotencyKey)
		})
	if err != nil {
		return nil, err
	}
	view.Replayed = replayed
	return view, nil
}

func (uc *MovementUseCase) postTransfer(ctx context.Context, repos Repos, fromID, toID domain.ID, amount domain.Amount, reference, idempotencyKey string) (*MovementView, string, error) {
	locked, err := uc.lockWallets(ctx, repos, fromID, toID)
	if err != nil {
		return nil, "", err
	}
	from, to := locked[fromID], locked[toID]

	postings, err := domain.PostTransfer(from, to, amount)
	if err != nil {
		return nil, "", err
	}
	if err := domain.AssertBalanced(postings); err != nil {
		return nil, "", err
	}

	metadata := map[string]any{
		"from_wallet_id": from.ID.String(),
		"to_wallet_id":   to.ID.String(),
	}
	txn, err := uc.commitMovement(ctx, repos, domain.TransactionTypeTransfer, reference, idempotencyKey,
		metadata, postings, []*domain.Wallet{from, to})
	if err != nil {
		return nil, "", err
	}

	view := movementToView(txn, from, amount, to.ID.String())
	return &view, txn.ID.String(), nil
}

// lockWallets locks every involved wallet in sorted ID order.
//
// This canonical order is what prevents deadlock: two transactions touching the
// same wallet pair always request locks in the same order, so no wait cycle can
// form between transactions.
func (uc *MovementUseCase) lockWallets(ctx context.Context, repos Repos, ids ...domain.ID) (map[domain.ID]*domain.Wallet, error) {
	ordered := make([]domain.ID, 0, len(ids))
	seen := make(map[domain.ID]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)

	locked := make(map[domain.ID]*domain.Wallet, len(ordered))
	for _, id := range ordered {
		w, err := repos.Wallets().GetByIDForUpdate(ctx, id)
		if err != nil {
			return nil, err
		}
		locked[id] = w
	}
	return locked, nil
}

// commitMovement persists every effect of one money movement: the wallet
// balances (already mutated in memory by the domain), the journal header, and
// the ledger rows. They all use the caller's database transaction, so a partial
// commit is impossible.
func (uc *MovementUseCase) commitMovement(
	ctx context.Context,
	repos Repos,
	txnType domain.TransactionType,
	reference, idempotencyKey string,
	metadata map[string]any,
	postings []domain.Posting,
	wallets []*domain.Wallet,
) (*domain.Transaction, error) {
	now := uc.clock()

	rawID, err := uc.newID()
	if err != nil {
		return nil, fmt.Errorf("generate transaction id: %w", err)
	}
	txnID, err := domain.ParseID("transaction_id", rawID)
	if err != nil {
		return nil, fmt.Errorf("generate transaction id: %w", err)
	}

	txn := domain.NewTransaction(txnType, reference, idempotencyKey, metadata, now)
	txn.ID = txnID

	// Balances are written first: if the balance invariant is violated (e.g. a
	// negative balance) the request fails fast before the journal is written.
	for _, w := range wallets {
		w.UpdatedAt = now.UTC()
		if err := repos.Wallets().Update(ctx, w); err != nil {
			return nil, err
		}
	}
	if err := repos.Ledger().CreateTransaction(ctx, txn); err != nil {
		return nil, err
	}

	entries := make([]domain.Entry, 0, len(postings))
	for _, p := range postings {
		entries = append(entries, p.Entry(txn.ID, now))
	}
	if err := repos.Ledger().InsertEntries(ctx, entries); err != nil {
		return nil, err
	}

	return txn, nil
}

func movementToView(txn *domain.Transaction, wallet *domain.Wallet, amount domain.Amount, counterparty string) MovementView {
	return MovementView{
		TransactionID:        txn.ID.String(),
		Type:                 string(txn.Type),
		Status:               string(txn.Status),
		WalletID:             wallet.ID.String(),
		AmountMinor:          amount.Int64(),
		Currency:             wallet.Currency.String(),
		BalanceAfter:         wallet.Balance.Int64(),
		Reference:            txn.Reference,
		CreatedAt:            txn.CreatedAt.UTC(),
		CounterpartyWalletID: counterparty,
	}
}

// parseMovementInput validates the input shared by top-up and withdraw.
func parseMovementInput(rawWalletID string, rawAmount int64, rawReference string) (domain.ID, domain.Amount, string, error) {
	walletID, err := domain.ParseID("wallet_id", rawWalletID)
	if err != nil {
		return "", 0, "", err
	}
	amount, err := domain.NewAmount("amount_minor", rawAmount)
	if err != nil {
		return "", 0, "", err
	}
	reference, err := normalizeReference(rawReference)
	if err != nil {
		return "", 0, "", err
	}
	return walletID, amount, reference, nil
}
