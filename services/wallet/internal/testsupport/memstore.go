package testsupport

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// MemStore is application.Store in memory. Transactions are serialised and roll back by restoring
// a snapshot, which is enough to test the core; the row locks of the real store are modelled by
// that serialisation, and tested against PostgreSQL itself.
type MemStore struct {
	txMu sync.Mutex // one transaction at a time
	mu   sync.Mutex // guards the maps

	wallets    map[string]domain.Wallet
	accounts   map[string]domain.Account
	operations map[string]domain.Operation
	holds      map[string]domain.Hold

	// FailSaveOperation, if set, is called before every SaveOperation and may make it fail, to
	// simulate the database going away after a ledger call.
	FailSaveOperation func(domain.Operation) error
}

var _ application.Store = (*MemStore)(nil)

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{
		wallets:    map[string]domain.Wallet{},
		accounts:   map[string]domain.Account{},
		operations: map[string]domain.Operation{},
		holds:      map[string]domain.Hold{},
	}
}

// Do implements application.Store.
func (m *MemStore) Do(ctx context.Context, fn func(context.Context, application.Repository) error) error {
	m.txMu.Lock()
	defer m.txMu.Unlock()

	m.mu.Lock()
	wallets, accounts := maps.Clone(m.wallets), maps.Clone(m.accounts)
	operations, holds := maps.Clone(m.operations), maps.Clone(m.holds)
	m.mu.Unlock()

	if err := fn(ctx, m); err != nil {
		m.mu.Lock()
		m.wallets, m.accounts, m.operations, m.holds = wallets, accounts, operations, holds
		m.mu.Unlock()

		return err
	}

	return nil
}

func (m *MemStore) EnsureWallet(_ context.Context, w domain.Wallet) (domain.Wallet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, existing := range m.wallets {
		if existing.UserID == w.UserID {
			return existing, nil
		}
	}
	m.wallets[w.ID] = w

	return w, nil
}

func (m *MemStore) WalletByUser(_ context.Context, userID string) (domain.Wallet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, w := range m.wallets {
		if w.UserID == userID {
			return w, nil
		}
	}

	return domain.Wallet{}, domain.ErrNotFound
}

func (m *MemStore) WalletByID(_ context.Context, id string) (domain.Wallet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.wallets[id]
	if !ok {
		return domain.Wallet{}, domain.ErrNotFound
	}

	return w, nil
}

func (m *MemStore) LockWallet(ctx context.Context, id string) (domain.Wallet, error) {
	return m.WalletByID(ctx, id)
}

func (m *MemStore) SaveWallet(_ context.Context, w domain.Wallet) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.wallets[w.ID]; !ok {
		return domain.ErrNotFound
	}
	m.wallets[w.ID] = w

	return nil
}

func (m *MemStore) InsertAccount(_ context.Context, a domain.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.accounts[a.ID] = a

	return nil
}

func (m *MemStore) AccountByID(_ context.Context, id string) (domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.accounts[id]
	if !ok {
		return domain.Account{}, domain.ErrNotFound
	}

	return a, nil
}

func (m *MemStore) LockAccounts(_ context.Context, ids ...string) (map[string]domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]domain.Account{}
	for _, id := range ids {
		a, ok := m.accounts[id]
		if !ok {
			return nil, domain.ErrNotFound
		}
		out[id] = a
	}

	return out, nil
}

func (m *MemStore) AccountsByWallet(_ context.Context, walletID string) ([]domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []domain.Account
	for _, a := range m.accounts {
		if a.WalletID == walletID {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b domain.Account) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})

	return out, nil
}

func (m *MemStore) SaveAccount(_ context.Context, a domain.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.accounts[a.ID]; !ok {
		return domain.ErrNotFound
	}
	m.accounts[a.ID] = a

	return nil
}

func (m *MemStore) InsertOperation(_ context.Context, op domain.Operation) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, existing := range m.operations {
		if existing.Initiator == op.Initiator && existing.IdempotencyKey == op.IdempotencyKey {
			return domain.ErrDuplicateKey
		}
	}
	m.operations[op.ID] = op

	return nil
}

func (m *MemStore) OperationByID(_ context.Context, id string) (domain.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	op, ok := m.operations[id]
	if !ok {
		return domain.Operation{}, domain.ErrNotFound
	}

	return op, nil
}

func (m *MemStore) OperationByKey(_ context.Context, initiator, key string) (domain.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, op := range m.operations {
		if op.Initiator == initiator && op.IdempotencyKey == key {
			return op, nil
		}
	}

	return domain.Operation{}, domain.ErrNotFound
}

func (m *MemStore) LockOperation(ctx context.Context, id string) (domain.Operation, error) {
	return m.OperationByID(ctx, id)
}

func (m *MemStore) SaveOperation(_ context.Context, op domain.Operation) error {
	if m.FailSaveOperation != nil {
		if err := m.FailSaveOperation(op); err != nil {
			return err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.operations[op.ID]; !ok {
		return domain.ErrNotFound
	}
	m.operations[op.ID] = op

	return nil
}

func (m *MemStore) ListAccountOperations(_ context.Context, accountID string, before *application.OperationCursor, limit int) ([]domain.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []domain.Operation
	for _, op := range m.operations {
		if !op.Touches(accountID) {
			continue
		}
		if before != nil {
			if c := op.CreatedAt.Compare(before.CreatedAt); c > 0 || (c == 0 && op.ID >= before.ID) {
				continue
			}
		}
		out = append(out, op)
	}
	slices.SortFunc(out, func(a, b domain.Operation) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

func spendCounts(op domain.Operation) bool {
	return op.Spending && op.ExecutedAt != nil &&
		(op.Status == domain.StatusPending || op.Status == domain.StatusSettling || op.Status == domain.StatusCompleted)
}

func (m *MemStore) SpentSince(_ context.Context, accountID string, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var sum int64
	for _, op := range m.operations {
		if spendCounts(op) && op.AccountID == accountID && !op.ExecutedAt.Before(since) {
			sum += op.Amount
		}
	}

	return sum, nil
}

func (m *MemStore) WalletSpentSince(_ context.Context, walletID string, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var sum int64
	for _, op := range m.operations {
		if spendCounts(op) && m.accounts[op.AccountID].WalletID == walletID && !op.ExecutedAt.Before(since) {
			sum += op.Amount
		}
	}

	return sum, nil
}

func (m *MemStore) PendingOutgoing(_ context.Context, accountID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var sum int64
	for _, op := range m.operations {
		if op.Status == domain.StatusPending && op.Kind.MovesMoneyOut() && op.AccountID == accountID {
			sum += op.Amount
		}
	}

	return sum, nil
}

func (m *MemStore) InFlightCount(_ context.Context, accountID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for _, op := range m.operations {
		if op.Status.InFlight() && op.Touches(accountID) {
			n++
		}
	}

	return n, nil
}

func (m *MemStore) ClaimDue(_ context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var due []domain.Operation
	for _, op := range m.operations {
		if (op.Status == domain.StatusPending || op.Status == domain.StatusSettling) && !op.NextAttemptAt.After(now) {
			due = append(due, op)
		}
	}
	slices.SortFunc(due, func(a, b domain.Operation) int {
		return cmp.Or(a.NextAttemptAt.Compare(b.NextAttemptAt), cmp.Compare(a.ID, b.ID))
	})
	if len(due) > limit {
		due = due[:limit]
	}

	for i, op := range due {
		op.Attempts++
		op.NextAttemptAt = now.Add(lease * time.Duration(min(op.Attempts, 20)))
		m.operations[op.ID] = op
		due[i] = op
	}

	return due, nil
}

func (m *MemStore) DueConfirmations(_ context.Context, now time.Time, limit int) ([]domain.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var due []domain.Operation
	for _, op := range m.operations {
		if op.Expired(now) {
			due = append(due, op)
		}
	}
	slices.SortFunc(due, func(a, b domain.Operation) int {
		return cmp.Or(a.ConfirmExpiresAt.Compare(*b.ConfirmExpiresAt), cmp.Compare(a.ID, b.ID))
	})
	if len(due) > limit {
		due = due[:limit]
	}

	return due, nil
}

func (m *MemStore) InsertHold(_ context.Context, h domain.Hold) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, existing := range m.holds {
		if existing.Initiator == h.Initiator && existing.IdempotencyKey == h.IdempotencyKey {
			return domain.ErrDuplicateKey
		}
	}
	m.holds[h.ID] = h

	return nil
}

func (m *MemStore) HoldByID(_ context.Context, id string) (domain.Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	h, ok := m.holds[id]
	if !ok {
		return domain.Hold{}, domain.ErrNotFound
	}

	return h, nil
}

func (m *MemStore) HoldByKey(_ context.Context, initiator, key string) (domain.Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, h := range m.holds {
		if h.Initiator == initiator && h.IdempotencyKey == key {
			return h, nil
		}
	}

	return domain.Hold{}, domain.ErrNotFound
}

func (m *MemStore) LockHold(ctx context.Context, id string) (domain.Hold, error) {
	return m.HoldByID(ctx, id)
}

func (m *MemStore) SaveHold(_ context.Context, h domain.Hold) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.holds[h.ID]; !ok {
		return domain.ErrNotFound
	}
	m.holds[h.ID] = h

	return nil
}

func (m *MemStore) ActiveHeld(_ context.Context, accountID string, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var sum int64
	for _, h := range m.holds {
		if h.AccountID == accountID && h.Active(now) {
			sum += h.Amount
		}
	}

	return sum, nil
}

func (m *MemStore) ActiveHoldCount(_ context.Context, accountID string, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for _, h := range m.holds {
		if h.AccountID == accountID && h.Active(now) {
			n++
		}
	}

	return n, nil
}

func (m *MemStore) ReleaseCapture(_ context.Context, operationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, h := range m.holds {
		if h.CapturedBy == operationID {
			h.CapturedBy = ""
			m.holds[id] = h
		}
	}

	return nil
}
