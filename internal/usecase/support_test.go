package usecase_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/example/wallet-service/internal/domain"
	"github.com/example/wallet-service/internal/usecase"
)

// This file contains in-memory fakes for the usecase ports.
//
// The fakes deliberately behave like a real database:
//   - Get* returns a COPY, so entity mutations are only persisted when Update()
//     is called (a "forgot to update the balance" bug gets caught);
//   - WithTx takes a snapshot and restores it when fn returns an error
//     (tests atomicity without PostgreSQL);
//   - WithTx is serialized with a mutex, mimicking the row lock between transactions.

const (
	systemWalletID = "00000000-0000-0000-0000-000000000001"
	ownerA         = "11111111-1111-4111-8111-111111111111"
	ownerB         = "22222222-2222-4222-8222-222222222222"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// idSeq produces valid, deterministic, sequential UUIDs.
type idSeq struct {
	mu sync.Mutex
	n  int64
}

func (s *idSeq) next() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.n), nil
}

type fakeStore struct {
	wallets     map[domain.ID]*domain.Wallet
	txns        []*domain.Transaction
	entries     []domain.Entry
	idempotency map[string]*domain.IdempotencyRecord
	nextEntryID int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		wallets:     map[domain.ID]*domain.Wallet{},
		idempotency: map[string]*domain.IdempotencyRecord{},
	}
}

func (s *fakeStore) clone() *fakeStore {
	c := &fakeStore{
		wallets:     make(map[domain.ID]*domain.Wallet, len(s.wallets)),
		txns:        append([]*domain.Transaction(nil), s.txns...),
		entries:     append([]domain.Entry(nil), s.entries...),
		idempotency: make(map[string]*domain.IdempotencyRecord, len(s.idempotency)),
		nextEntryID: s.nextEntryID,
	}
	for k, v := range s.wallets {
		cp := *v
		c.wallets[k] = &cp
	}
	for k, v := range s.idempotency {
		cp := *v
		c.idempotency[k] = &cp
	}
	return c
}

func (s *fakeStore) restore(other *fakeStore) { *s = *other }

type fakeUoW struct {
	mu    sync.Mutex
	store *fakeStore
}

func (u *fakeUoW) WithTx(ctx context.Context, fn func(ctx context.Context, repos usecase.Repos) error) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	snapshot := u.store.clone()
	if err := fn(ctx, &fakeRepos{store: u.store}); err != nil {
		u.store.restore(snapshot)
		return err
	}
	return nil
}

func (u *fakeUoW) ReadRepos() usecase.Repos { return &fakeRepos{store: u.store} }

type fakeRepos struct{ store *fakeStore }

func (r *fakeRepos) Wallets() usecase.WalletRepository { return &fakeWalletRepo{store: r.store} }
func (r *fakeRepos) Ledger() usecase.LedgerRepository  { return &fakeLedgerRepo{store: r.store} }
func (r *fakeRepos) Idempotency() usecase.IdempotencyRepository {
	return &fakeIdempotencyRepo{store: r.store}
}

type fakeWalletRepo struct{ store *fakeStore }

func (r *fakeWalletRepo) Create(ctx context.Context, w *domain.Wallet) error {
	if _, ok := r.store.wallets[w.ID]; ok {
		return fmt.Errorf("wallet %s already exists", w.ID)
	}
	cp := *w
	r.store.wallets[w.ID] = &cp
	return nil
}

func (r *fakeWalletRepo) GetByID(ctx context.Context, id domain.ID) (*domain.Wallet, error) {
	w, ok := r.store.wallets[id]
	if !ok {
		return nil, &domain.NotFoundError{Resource: "wallet", ID: id.String()}
	}
	cp := *w
	return &cp, nil
}

// GetByIDForUpdate is identical to GetByID because serialization between
// "transactions" is already guaranteed by the fakeUoW mutex.
func (r *fakeWalletRepo) GetByIDForUpdate(ctx context.Context, id domain.ID) (*domain.Wallet, error) {
	return r.GetByID(ctx, id)
}

func (r *fakeWalletRepo) Update(ctx context.Context, w *domain.Wallet) error {
	if _, ok := r.store.wallets[w.ID]; !ok {
		return &domain.NotFoundError{Resource: "wallet", ID: w.ID.String()}
	}
	cp := *w
	r.store.wallets[w.ID] = &cp
	return nil
}

type fakeLedgerRepo struct{ store *fakeStore }

func (r *fakeLedgerRepo) CreateTransaction(ctx context.Context, t *domain.Transaction) error {
	cp := *t
	r.store.txns = append(r.store.txns, &cp)
	return nil
}

func (r *fakeLedgerRepo) InsertEntries(ctx context.Context, entries []domain.Entry) error {
	for _, e := range entries {
		r.store.nextEntryID++
		e.ID = r.store.nextEntryID
		r.store.entries = append(r.store.entries, e)
	}
	return nil
}

func (r *fakeLedgerRepo) ListRecordsByWallet(ctx context.Context, walletID domain.ID, afterEntryID int64, limit int) ([]domain.LedgerRecord, error) {
	records := make([]domain.LedgerRecord, 0, limit)
	// iterate backwards = id DESC order (newest first).
	for i := len(r.store.entries) - 1; i >= 0 && len(records) < limit; i-- {
		e := r.store.entries[i]
		if e.WalletID != walletID {
			continue
		}
		if afterEntryID > 0 && e.ID >= afterEntryID {
			continue
		}
		records = append(records, r.toRecord(e))
	}
	return records, nil
}

func (r *fakeLedgerRepo) toRecord(e domain.Entry) domain.LedgerRecord {
	rec := domain.LedgerRecord{
		EntryID:       e.ID,
		TransactionID: e.TransactionID,
		Direction:     e.Direction,
		Amount:        e.Amount,
		BalanceAfter:  e.BalanceAfter,
		Currency:      e.Currency,
		CreatedAt:     e.CreatedAt,
	}
	for _, t := range r.store.txns {
		if t.ID == e.TransactionID {
			rec.Type = t.Type
			rec.Reference = t.Reference
			break
		}
	}
	for _, other := range r.store.entries {
		if other.TransactionID == e.TransactionID && other.WalletID != e.WalletID {
			cp := other.WalletID
			rec.CounterpartyWalletID = &cp
			break
		}
	}
	return rec
}

type fakeIdempotencyRepo struct{ store *fakeStore }

func (r *fakeIdempotencyRepo) Claim(ctx context.Context, rec *domain.IdempotencyRecord) (domain.ClaimResult, error) {
	if existing, ok := r.store.idempotency[rec.Key]; ok {
		cp := *existing
		return domain.ClaimResult{Claimed: false, Existing: &cp}, nil
	}
	cp := *rec
	r.store.idempotency[rec.Key] = &cp
	return domain.ClaimResult{Claimed: true}, nil
}

func (r *fakeIdempotencyRepo) Complete(ctx context.Context, key string, statusCode int, body []byte, resourceID *domain.ID) error {
	rec, ok := r.store.idempotency[key]
	if !ok {
		return fmt.Errorf("idempotency record %q not found", key)
	}
	rec.Status = domain.IdempotencyStatusCompleted
	rec.StatusCode = statusCode
	rec.ResponseBody = body
	rec.ResourceID = resourceID
	return nil
}

// testEnv assembles the usecases with fakes.
type testEnv struct {
	uow          *fakeUoW
	store        *fakeStore
	wallets      *usecase.WalletUseCase
	movement     *usecase.MovementUseCase
	topUpCounter int
}

func newTestEnv() *testEnv {
	store := newFakeStore()
	sysID := domain.MustParseID(systemWalletID)
	store.wallets[sysID] = &domain.Wallet{
		ID:        sysID,
		OwnerID:   domain.NilID,
		Currency:  domain.CurrencyIDR,
		Status:    domain.WalletStatusActive,
		IsSystem:  true,
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
	}

	clock := func() time.Time { return fixedNow }
	uow := &fakeUoW{store: store}
	ids := &idSeq{}

	return &testEnv{
		uow:      uow,
		store:    store,
		wallets:  usecase.NewWalletUseCase(uow, clock, ids.next, time.Hour),
		movement: usecase.NewMovementUseCase(uow, clock, ids.next, sysID, time.Hour),
	}
}

func (e *testEnv) createWallet(owner string) string {
	view, err := e.wallets.CreateWallet(context.Background(), usecase.CreateWalletInput{
		OwnerID:  owner,
		Currency: "IDR",
	})
	if err != nil {
		panic(fmt.Sprintf("createWallet: %v", err))
	}
	return view.ID
}

// topUp performs a top-up and fails hard on error. When key is empty, a unique
// idempotency key is generated automatically (deterministic counter).
func (e *testEnv) topUp(walletID string, minor int64, key string) *usecase.MovementView {
	if key == "" {
		e.topUpCounter++
		key = fmt.Sprintf("auto-topup-key-%04d", e.topUpCounter)
	}
	view, err := e.movement.TopUp(context.Background(), usecase.TopUpInput{
		WalletID:       walletID,
		AmountMinor:    minor,
		IdempotencyKey: key,
	})
	if err != nil {
		panic(fmt.Sprintf("topUp(%s, %d): %v", walletID, minor, err))
	}
	return view
}

func (e *testEnv) balanceOf(walletID string) int64 {
	w, err := e.wallets.GetWallet(context.Background(), usecase.GetWalletInput{WalletID: walletID})
	if err != nil {
		panic(err)
	}
	return w.BalanceMinor
}

func (e *testEnv) ledgerEntryCount(walletID string) int {
	n := 0
	for _, entry := range e.store.entries {
		if entry.WalletID.String() == walletID {
			n++
		}
	}
	return n
}
