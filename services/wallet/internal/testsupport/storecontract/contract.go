// Package storecontract is one suite of tests for application.Store, run against both the
// in-memory fake and the PostgreSQL adapter. The fake is only worth having if it behaves like the
// real thing, and this is how that is kept true.
package storecontract

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

var base = time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)

// Factory returns a new, empty store.
type Factory func(t *testing.T) application.Store

// Run runs the whole suite against stores made by newStore.
func Run(t *testing.T, newStore Factory) {
	suites := map[string]func(*testing.T, application.Store){
		"wallets":                 wallets,
		"accounts":                accounts,
		"operations round trip":   operationRoundTrip,
		"idempotency keys":        operationKeys,
		"listing operations":      listing,
		"spending totals":         spending,
		"pending and in flight":   pendingAndInFlight,
		"claiming due operations": claiming,
		"due confirmations":       confirmations,
		"holds":                   holds,
		"transactions":            transactions,
	}

	for name, suite := range suites {
		t.Run(name, func(t *testing.T) { suite(t, newStore(t)) })
	}
}

var ctx = context.Background()

func must(t testing.TB, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func is(t testing.TB, err, target error) {
	t.Helper()

	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want it to wrap %v", err, target)
	}
}

func i64(v int64) *int64 { return &v }

func at(offset time.Duration) time.Time { return base.Add(offset) }

func tp(t time.Time) *time.Time { return &t }

// seed makes a wallet with two accounts, and a second wallet with one.
type fixture struct {
	wallet, other domain.Wallet
	a, b, c       domain.Account // a and b in wallet, c in other
}

func seed(t testing.TB, s application.Store) fixture {
	t.Helper()

	f := fixture{
		wallet: domain.Wallet{ID: testsupport.UUID(1), UserID: testsupport.UUID(11), CreatedAt: base, UpdatedAt: base},
		other:  domain.Wallet{ID: testsupport.UUID(2), UserID: testsupport.UUID(12), CreatedAt: base, UpdatedAt: base},
	}
	for _, w := range []domain.Wallet{f.wallet, f.other} {
		if _, err := s.EnsureWallet(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	f.a = domain.Account{ID: testsupport.UUID(21), WalletID: f.wallet.ID, Name: "a", Status: domain.AccountActive, CreatedAt: at(0), UpdatedAt: at(0)}
	f.b = domain.Account{ID: testsupport.UUID(22), WalletID: f.wallet.ID, Name: "b", Status: domain.AccountActive, CreatedAt: at(time.Second), UpdatedAt: at(time.Second)}
	f.c = domain.Account{ID: testsupport.UUID(23), WalletID: f.other.ID, Name: "c", Status: domain.AccountActive, CreatedAt: at(2 * time.Second), UpdatedAt: at(2 * time.Second)}
	for _, a := range []domain.Account{f.a, f.b, f.c} {
		must(t, s.InsertAccount(ctx, a))
	}

	return f
}

// op builds a valid pending deposit; tests change what they care about.
func op(n int, account string) domain.Operation {
	return domain.Operation{
		ID: testsupport.UUID(1000 + n), Kind: domain.KindDeposit, AccountID: account, Amount: 100,
		Status: domain.StatusPending, Initiator: "user:" + testsupport.UUID(11), IdempotencyKey: testsupport.UUID(2000 + n),
		RequestHash: "hash", DeadlineAt: at(24 * time.Hour), NextAttemptAt: at(time.Minute),
		CreatedAt: at(time.Duration(n) * time.Minute), UpdatedAt: at(time.Duration(n) * time.Minute),
		ExecutedAt: tp(at(time.Duration(n) * time.Minute)),
	}
}

// withdrawal builds a valid spending withdrawal in any status.
func withdrawal(n int, account string, amount int64, status domain.OperationStatus) domain.Operation {
	o := op(n, account)
	o.Kind, o.Amount, o.Status, o.Spending = domain.KindWithdrawal, amount, status, true

	switch status {
	case domain.StatusCompleted:
		o.CompletedAt = tp(at(time.Minute))
	case domain.StatusFailed:
		o.Failure, o.CompletedAt = domain.FailureRejected, tp(at(time.Minute))
	case domain.StatusAwaitingConfirmation:
		o.LimitScope, o.ConfirmExpiresAt, o.ExecutedAt = domain.ScopeAccount, tp(at(time.Hour)), nil
	}

	return o
}

func insert(t testing.TB, s application.Store, ops ...domain.Operation) {
	t.Helper()

	for _, o := range ops {
		must(t, s.InsertOperation(ctx, o))
	}
}

func wallets(t *testing.T, s application.Store) {
	f := seed(t, s)

	// EnsureWallet returns the user's existing wallet, ignoring the new id.
	again, err := s.EnsureWallet(ctx, domain.Wallet{ID: testsupport.UUID(99), UserID: f.wallet.UserID, CreatedAt: at(time.Hour), UpdatedAt: at(time.Hour)})
	must(t, err)
	if again.ID != f.wallet.ID || !again.CreatedAt.Equal(base) {
		t.Errorf("EnsureWallet returned %+v, want the original wallet", again)
	}

	for name, got := range map[string]func() (domain.Wallet, error){
		"by user": func() (domain.Wallet, error) { return s.WalletByUser(ctx, f.wallet.UserID) },
		"by id":   func() (domain.Wallet, error) { return s.WalletByID(ctx, f.wallet.ID) },
		"locked":  func() (domain.Wallet, error) { return s.LockWallet(ctx, f.wallet.ID) },
	} {
		w, err := got()
		must(t, err)
		if w.ID != f.wallet.ID || w.UserID != f.wallet.UserID || w.DailyLimit != nil || !w.CreatedAt.Equal(base) {
			t.Errorf("%s: %+v", name, w)
		}
	}

	_, err = s.WalletByUser(ctx, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)
	_, err = s.WalletByID(ctx, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)
	_, err = s.LockWallet(ctx, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)

	// The limit round-trips, and clears.
	f.wallet.DailyLimit, f.wallet.UpdatedAt = i64(5000), at(time.Minute)
	must(t, s.SaveWallet(ctx, f.wallet))
	got, err := s.WalletByID(ctx, f.wallet.ID)
	must(t, err)
	if got.DailyLimit == nil || *got.DailyLimit != 5000 || !got.UpdatedAt.Equal(at(time.Minute)) {
		t.Errorf("after save: %+v", got)
	}
	f.wallet.DailyLimit = nil
	must(t, s.SaveWallet(ctx, f.wallet))
	if got, _ = s.WalletByID(ctx, f.wallet.ID); got.DailyLimit != nil {
		t.Errorf("limit not cleared: %v", *got.DailyLimit)
	}
	is(t, s.SaveWallet(ctx, domain.Wallet{ID: testsupport.UUID(500), UpdatedAt: base}), domain.ErrNotFound)
}

func accounts(t *testing.T, s application.Store) {
	f := seed(t, s)

	got, err := s.AccountByID(ctx, f.a.ID)
	must(t, err)
	if got.ID != f.a.ID || got.WalletID != f.wallet.ID || got.Name != "a" || got.Status != domain.AccountActive || got.DailyLimit != nil || got.ClosedAt != nil {
		t.Errorf("account = %+v", got)
	}
	_, err = s.AccountByID(ctx, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)

	list, err := s.AccountsByWallet(ctx, f.wallet.ID)
	must(t, err)
	if len(list) != 2 || list[0].ID != f.a.ID || list[1].ID != f.b.ID {
		t.Errorf("accounts by wallet = %+v, want a then b", list)
	}
	if list, _ = s.AccountsByWallet(ctx, testsupport.UUID(500)); len(list) != 0 {
		t.Errorf("accounts of an unknown wallet = %+v", list)
	}

	// Lock returns them by id, whatever order they were named in; a missing one fails the lot.
	locked, err := s.LockAccounts(ctx, f.b.ID, f.a.ID)
	must(t, err)
	if len(locked) != 2 || locked[f.a.ID].Name != "a" || locked[f.b.ID].Name != "b" {
		t.Errorf("locked = %+v", locked)
	}
	_, err = s.LockAccounts(ctx, f.a.ID, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)

	closed := at(time.Hour)
	f.a.Name, f.a.Status, f.a.DailyLimit, f.a.UpdatedAt, f.a.ClosedAt = "renamed", domain.AccountClosed, i64(700), closed, &closed
	must(t, s.SaveAccount(ctx, f.a))
	got, _ = s.AccountByID(ctx, f.a.ID)
	if got.Name != "renamed" || got.Status != domain.AccountClosed || got.DailyLimit == nil || *got.DailyLimit != 700 || got.ClosedAt == nil || !got.ClosedAt.Equal(closed) {
		t.Errorf("after save: %+v", got)
	}
	is(t, s.SaveAccount(ctx, domain.Account{ID: testsupport.UUID(500), Name: "x", Status: domain.AccountActive, UpdatedAt: base}), domain.ErrNotFound)
}

func operationRoundTrip(t *testing.T, s application.Store) {
	f := seed(t, s)

	full := domain.Operation{
		ID: testsupport.UUID(1001), Kind: domain.KindTransfer, AccountID: f.a.ID, CounterpartyID: f.c.ID, Amount: 4321,
		Status: domain.StatusAwaitingConfirmation, LimitScope: domain.ScopeWallet,
		Initiator: "service:payment", IdempotencyKey: testsupport.UUID(2001), RequestHash: "abc",
		Spending: true, ConfirmExpiresAt: tp(at(10 * time.Minute)), DeadlineAt: at(48 * time.Hour),
		Attempts: 3, NextAttemptAt: at(time.Hour), CreatedAt: at(time.Second), UpdatedAt: at(2 * time.Second),
	}
	must(t, s.InsertOperation(ctx, full))

	got, err := s.OperationByID(ctx, full.ID)
	must(t, err)
	if !same(got, full) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, full)
	}

	// Saving writes the columns that change; the status machine's whole range round-trips.
	full.Status, full.LimitScope, full.ConfirmExpiresAt = domain.StatusFailed, domain.ScopeWallet, nil
	full.Failure, full.OverLimitConfirmed, full.Attempts = domain.FailureCancelled, true, 4
	full.ExecutedAt, full.RefundedAt, full.CompletedAt = tp(at(3*time.Second)), tp(at(4*time.Second)), tp(at(5*time.Second))
	full.UpdatedAt, full.NextAttemptAt, full.DeadlineAt = at(6*time.Second), at(7*time.Second), at(8*time.Second)
	must(t, s.SaveOperation(ctx, full))

	if got, err = s.OperationByID(ctx, full.ID); err != nil || !same(got, full) {
		t.Errorf("after save:\n got %+v\nwant %+v (%v)", got, full, err)
	}

	locked, err := s.LockOperation(ctx, full.ID)
	must(t, err)
	if locked.ID != full.ID {
		t.Errorf("locked %q", locked.ID)
	}

	_, err = s.OperationByID(ctx, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)
	_, err = s.LockOperation(ctx, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)
	missing := op(9, f.a.ID)
	is(t, s.SaveOperation(ctx, missing), domain.ErrNotFound)
}

func same(a, b domain.Operation) bool {
	eq := func(x, y *time.Time) bool { return (x == nil && y == nil) || (x != nil && y != nil && x.Equal(*y)) }

	return a.ID == b.ID && a.Kind == b.Kind && a.AccountID == b.AccountID && a.CounterpartyID == b.CounterpartyID &&
		a.Amount == b.Amount && a.HoldID == b.HoldID && a.Status == b.Status && a.Failure == b.Failure &&
		a.LimitScope == b.LimitScope && a.Initiator == b.Initiator && a.IdempotencyKey == b.IdempotencyKey &&
		a.RequestHash == b.RequestHash && a.Spending == b.Spending && a.OverLimitConfirmed == b.OverLimitConfirmed &&
		eq(a.ConfirmExpiresAt, b.ConfirmExpiresAt) && a.DeadlineAt.Equal(b.DeadlineAt) && a.Attempts == b.Attempts &&
		a.NextAttemptAt.Equal(b.NextAttemptAt) && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt) &&
		eq(a.ExecutedAt, b.ExecutedAt) && eq(a.RefundedAt, b.RefundedAt) && eq(a.CompletedAt, b.CompletedAt)
}

func operationKeys(t *testing.T, s application.Store) {
	f := seed(t, s)
	first := op(1, f.a.ID)
	insert(t, s, first)

	// The same initiator and key is a duplicate; another initiator with the same key is not.
	clash := op(2, f.a.ID)
	clash.IdempotencyKey = first.IdempotencyKey
	is(t, s.InsertOperation(ctx, clash), domain.ErrDuplicateKey)

	other := op(3, f.a.ID)
	other.Initiator, other.IdempotencyKey = "service:payment", first.IdempotencyKey
	must(t, s.InsertOperation(ctx, other))

	got, err := s.OperationByKey(ctx, first.Initiator, first.IdempotencyKey)
	must(t, err)
	if got.ID != first.ID {
		t.Errorf("OperationByKey = %s, want %s", got.ID, first.ID)
	}
	if got, err = s.OperationByKey(ctx, "service:payment", first.IdempotencyKey); err != nil || got.ID != other.ID {
		t.Errorf("OperationByKey for the other initiator = %s, %v", got.ID, err)
	}
	_, err = s.OperationByKey(ctx, first.Initiator, testsupport.UUID(500))
	is(t, err, domain.ErrNotFound)
}

func listing(t *testing.T, s application.Store) {
	f := seed(t, s)

	// Five operations on a, one of which is a transfer from c to a; one on b only.
	var ids []string
	for n := 1; n <= 5; n++ {
		o := op(n, f.a.ID)
		if n == 3 {
			o.Kind, o.AccountID, o.CounterpartyID, o.Spending = domain.KindTransfer, f.c.ID, f.a.ID, true
		}
		insert(t, s, o)
		ids = append([]string{o.ID}, ids...)
	}
	insert(t, s, op(6, f.b.ID))

	var got []string
	var cursor *application.OperationCursor
	for pages := 0; pages < 6; pages++ {
		page, err := s.ListAccountOperations(ctx, f.a.ID, cursor, 2)
		must(t, err)
		if len(page) == 0 {
			break
		}
		for _, o := range page {
			got = append(got, o.ID)
		}
		last := page[len(page)-1]
		cursor = &application.OperationCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(got) != 5 {
		t.Fatalf("listed %d operations, want 5: %v", len(got), got)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Errorf("position %d = %s, want %s (newest first, both sides included)", i, got[i], ids[i])
		}
	}

	// Operations created in the same instant are ordered by id, newest id first, and a cursor
	// splits them cleanly.
	tie1, tie2 := op(7, f.b.ID), op(8, f.b.ID)
	tie1.CreatedAt, tie2.CreatedAt = at(time.Hour), at(time.Hour)
	insert(t, s, tie1, tie2)
	page, err := s.ListAccountOperations(ctx, f.b.ID, nil, 1)
	must(t, err)
	if len(page) != 1 || page[0].ID != tie2.ID {
		t.Fatalf("first of the tie = %v, want %s", page, tie2.ID)
	}
	next, err := s.ListAccountOperations(ctx, f.b.ID, &application.OperationCursor{CreatedAt: page[0].CreatedAt, ID: page[0].ID}, 1)
	must(t, err)
	if len(next) != 1 || next[0].ID != tie1.ID {
		t.Errorf("second of the tie = %v, want %s", next, tie1.ID)
	}
}

func spending(t *testing.T, s application.Store) {
	f := seed(t, s)
	day := at(0)

	insert(t, s,
		withdrawal(1, f.a.ID, 100, domain.StatusCompleted),  // counts
		withdrawal(2, f.a.ID, 200, domain.StatusPending),    // counts
		withdrawal(3, f.a.ID, 400, domain.StatusSettling),   // counts
		withdrawal(4, f.b.ID, 800, domain.StatusCompleted),  // another account of the same wallet
		withdrawal(5, f.c.ID, 1600, domain.StatusCompleted), // another wallet
	)
	// None of these count: failed, awaiting, not spending, executed before the cutoff.
	notSpending := withdrawal(8, f.a.ID, 12800, domain.StatusCompleted)
	notSpending.Spending = false
	old := withdrawal(9, f.a.ID, 25600, domain.StatusCompleted)
	old.ExecutedAt = tp(day.Add(-time.Second))
	insert(t, s,
		withdrawal(6, f.a.ID, 3200, domain.StatusFailed),
		withdrawal(7, f.a.ID, 6400, domain.StatusAwaitingConfirmation),
		notSpending, old)

	if got, err := s.SpentSince(ctx, f.a.ID, day); err != nil || got != 700 {
		t.Errorf("SpentSince(a) = %d, %v; want 700", got, err)
	}
	if got, err := s.WalletSpentSince(ctx, f.wallet.ID, day); err != nil || got != 1500 {
		t.Errorf("WalletSpentSince = %d, %v; want 1500", got, err)
	}
	if got, err := s.SpentSince(ctx, f.a.ID, day.Add(time.Hour)); err != nil || got != 0 {
		t.Errorf("SpentSince after everything = %d, %v; want 0", got, err)
	}
	if got, err := s.SpentSince(ctx, f.a.ID, day.Add(-time.Second)); err != nil || got != 700+25600 {
		t.Errorf("SpentSince from before the old one = %d, %v; want %d", got, err, 700+25600)
	}
}

func pendingAndInFlight(t *testing.T, s application.Store) {
	f := seed(t, s)

	pendingOut := withdrawal(1, f.a.ID, 100, domain.StatusPending)
	settlingOut := withdrawal(2, f.a.ID, 200, domain.StatusSettling)
	pendingIn := op(3, f.a.ID) // a deposit: money in, not outgoing
	transfer := withdrawal(4, f.a.ID, 400, domain.StatusPending)
	transfer.Kind, transfer.CounterpartyID = domain.KindTransfer, f.c.ID
	awaiting := withdrawal(5, f.a.ID, 800, domain.StatusAwaitingConfirmation)
	done := withdrawal(6, f.a.ID, 1600, domain.StatusCompleted)
	insert(t, s, pendingOut, settlingOut, pendingIn, transfer, awaiting, done)

	// Only pending, outgoing operations are not yet in the ledger balance.
	if got, err := s.PendingOutgoing(ctx, f.a.ID); err != nil || got != 500 {
		t.Errorf("PendingOutgoing(a) = %d, %v; want 500", got, err)
	}
	if got, _ := s.PendingOutgoing(ctx, f.c.ID); got != 0 {
		t.Errorf("PendingOutgoing(c) = %d: a transfer is outgoing on its source only", got)
	}

	// In flight: awaiting, pending, settling, on either side.
	if got, err := s.InFlightCount(ctx, f.a.ID); err != nil || got != 5 {
		t.Errorf("InFlightCount(a) = %d, %v; want 5", got, err)
	}
	if got, _ := s.InFlightCount(ctx, f.c.ID); got != 1 {
		t.Errorf("InFlightCount(c) = %d, want 1 (the transfer to it)", got)
	}
	if got, _ := s.InFlightCount(ctx, f.b.ID); got != 0 {
		t.Errorf("InFlightCount(b) = %d, want 0", got)
	}
}

func claiming(t *testing.T, s application.Store) {
	f := seed(t, s)

	due1, due2, later := op(1, f.a.ID), op(2, f.a.ID), op(3, f.a.ID)
	due1.NextAttemptAt, due2.NextAttemptAt, later.NextAttemptAt = at(time.Minute), at(2*time.Minute), at(time.Hour)
	settling := op(4, f.a.ID)
	settling.Status, settling.NextAttemptAt = domain.StatusSettling, at(3*time.Minute)
	completed := withdrawal(5, f.a.ID, 100, domain.StatusCompleted)
	completed.NextAttemptAt = at(0)
	insert(t, s, due1, due2, later, settling, completed)

	now, lease := at(10*time.Minute), 5*time.Minute

	got, err := s.ClaimDue(ctx, now, lease, 2)
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("claimed %d, want 2 (limit)", len(got))
	}
	ids := map[string]domain.Operation{got[0].ID: got[0], got[1].ID: got[1]}
	if _, ok := ids[due1.ID]; !ok {
		t.Errorf("the oldest due operation was not claimed first: %v", ids)
	}
	for _, o := range got {
		if o.Attempts != 1 || !o.NextAttemptAt.Equal(now.Add(lease)) {
			t.Errorf("claimed %s: attempts %d, next %s; want 1 and %s", o.ID, o.Attempts, o.NextAttemptAt, now.Add(lease))
		}
		stored, _ := s.OperationByID(ctx, o.ID)
		if stored.Attempts != 1 || !stored.NextAttemptAt.Equal(now.Add(lease)) {
			t.Errorf("the claim was not stored: %+v", stored)
		}
	}

	// A second claim gets the rest that is due (the settling one), not the ones already claimed,
	// not the one that is not yet due, not the completed one.
	rest, err := s.ClaimDue(ctx, now, lease, 10)
	must(t, err)
	if len(rest) != 1 || rest[0].ID != settling.ID {
		t.Errorf("second claim = %v, want only the settling operation", rest)
	}
	if again, _ := s.ClaimDue(ctx, now, lease, 10); len(again) != 0 {
		t.Errorf("a third claim at the same instant returned %d operations", len(again))
	}

	// The lease grows with each attempt, to twenty times its base.
	later2 := now.Add(time.Hour)
	third, err := s.ClaimDue(ctx, later2, lease, 10)
	must(t, err)
	for _, o := range third {
		if o.Attempts != 2 || !o.NextAttemptAt.Equal(later2.Add(2*lease)) {
			if o.ID == later.ID && o.Attempts == 1 {
				continue // its first claim
			}
			t.Errorf("reclaimed %s: attempts %d, next %s; want 2 and %s", o.ID, o.Attempts, o.NextAttemptAt, later2.Add(2*lease))
		}
	}

	capped := op(6, f.a.ID)
	capped.Attempts = 40
	insert(t, s, capped)
	got, err = s.ClaimDue(ctx, now, lease, 10)
	must(t, err)
	for _, o := range got {
		if o.ID == capped.ID && !o.NextAttemptAt.Equal(now.Add(20*lease)) {
			t.Errorf("the lease must stop growing at 20 times: next %s", o.NextAttemptAt)
		}
	}
}

func confirmations(t *testing.T, s application.Store) {
	f := seed(t, s)

	awaiting := func(n int, expires time.Duration) domain.Operation {
		o := withdrawal(n, f.a.ID, 100, domain.StatusAwaitingConfirmation)
		o.ConfirmExpiresAt = tp(at(expires))

		return o
	}
	early, late := awaiting(1, time.Minute), awaiting(2, time.Hour)
	insert(t, s, early, late, op(3, f.a.ID))

	got, err := s.DueConfirmations(ctx, at(2*time.Minute), 10)
	must(t, err)
	if len(got) != 1 || got[0].ID != early.ID {
		t.Errorf("due at +2m = %v, want only the first", got)
	}
	if got, _ = s.DueConfirmations(ctx, at(time.Minute), 10); len(got) != 1 {
		t.Errorf("due exactly at the expiry = %d, want 1", len(got))
	}
	if got, _ = s.DueConfirmations(ctx, at(2*time.Hour), 1); len(got) != 1 || got[0].ID != early.ID {
		t.Errorf("limit 1 = %v, want the earliest", got)
	}
}

func holds(t *testing.T, s application.Store) {
	f := seed(t, s)
	hold := func(n int, account string, amount int64) domain.Hold {
		return domain.Hold{
			ID: testsupport.UUID(3000 + n), AccountID: account, Amount: amount, Reason: "order", ExpiresAt: at(time.Hour),
			Initiator: "service:payment", IdempotencyKey: testsupport.UUID(4000 + n), RequestHash: "h", CreatedAt: at(0),
		}
	}

	h := hold(1, f.a.ID, 100)
	must(t, s.InsertHold(ctx, h))
	clash := hold(2, f.a.ID, 5)
	clash.IdempotencyKey = h.IdempotencyKey
	is(t, s.InsertHold(ctx, clash), domain.ErrDuplicateKey)

	got, err := s.HoldByID(ctx, h.ID)
	must(t, err)
	if got.Amount != 100 || got.Reason != "order" || got.Initiator != "service:payment" || got.ReleasedAt != nil || got.CapturedBy != "" || !got.ExpiresAt.Equal(h.ExpiresAt) {
		t.Errorf("hold = %+v", got)
	}
	if got, err = s.HoldByKey(ctx, h.Initiator, h.IdempotencyKey); err != nil || got.ID != h.ID {
		t.Errorf("HoldByKey: %v, %v", got.ID, err)
	}
	if got, err = s.LockHold(ctx, h.ID); err != nil || got.ID != h.ID {
		t.Errorf("LockHold: %v, %v", got.ID, err)
	}
	for _, call := range []func() error{
		func() error { _, err := s.HoldByID(ctx, testsupport.UUID(500)); return err },
		func() error { _, err := s.HoldByKey(ctx, h.Initiator, testsupport.UUID(500)); return err },
		func() error { _, err := s.LockHold(ctx, testsupport.UUID(500)); return err },
		func() error { return s.SaveHold(ctx, hold(9, f.a.ID, 1)) },
	} {
		is(t, call(), domain.ErrNotFound)
	}

	// Active totals leave out released, captured and expired holds, and other accounts.
	released, captured, expired := hold(3, f.a.ID, 200), hold(4, f.a.ID, 400), hold(5, f.a.ID, 800)
	released.ReleasedAt = tp(at(time.Minute))
	expired.ExpiresAt = at(10 * time.Minute)
	otherAccount := hold(6, f.b.ID, 1600)
	for _, x := range []domain.Hold{released, captured, expired, otherAccount} {
		must(t, s.InsertHold(ctx, x))
	}
	capturing := withdrawal(1, f.a.ID, 400, domain.StatusPending)
	capturing.HoldID = captured.ID
	insert(t, s, capturing)
	captured.CapturedBy = capturing.ID
	must(t, s.SaveHold(ctx, captured))

	now := at(time.Hour / 2)
	if total, err := s.ActiveHeld(ctx, f.a.ID, now); err != nil || total != 100 {
		t.Errorf("ActiveHeld = %d, %v; want 100", total, err)
	}
	if n, err := s.ActiveHoldCount(ctx, f.a.ID, now); err != nil || n != 1 {
		t.Errorf("ActiveHoldCount = %d, %v; want 1", n, err)
	}
	if total, _ := s.ActiveHeld(ctx, f.a.ID, at(2*time.Hour)); total != 0 {
		t.Errorf("ActiveHeld after everything expired = %d", total)
	}
	if total, _ := s.ActiveHeld(ctx, f.a.ID, at(10*time.Minute-time.Microsecond)); total != 100+800 {
		t.Errorf("ActiveHeld just before a hold expires = %d, want 900", total)
	}
	if total, _ := s.ActiveHeld(ctx, f.a.ID, at(10*time.Minute)); total != 100 {
		t.Errorf("ActiveHeld at the expiry instant = %d, want 100: a hold is over when it expires", total)
	}

	// Undoing a capture makes the hold count again; releasing sticks.
	must(t, s.ReleaseCapture(ctx, capturing.ID))
	if got, _ = s.HoldByID(ctx, captured.ID); got.CapturedBy != "" {
		t.Errorf("capture not undone: %q", got.CapturedBy)
	}
	if total, _ := s.ActiveHeld(ctx, f.a.ID, now); total != 100+400 {
		t.Errorf("ActiveHeld after the capture was undone = %d, want 500", total)
	}
	must(t, s.ReleaseCapture(ctx, testsupport.UUID(500))) // nothing to undo is fine
}

func transactions(t *testing.T, s application.Store) {
	f := seed(t, s)

	// Writes inside Do are visible inside it, and committed when it returns nil.
	err := s.Do(ctx, func(ctx context.Context, r application.Repository) error {
		w, err := r.LockWallet(ctx, f.wallet.ID)
		if err != nil {
			return err
		}
		w.DailyLimit, w.UpdatedAt = i64(900), at(time.Minute)
		if err := r.SaveWallet(ctx, w); err != nil {
			return err
		}
		inside, err := r.WalletByID(ctx, f.wallet.ID)
		if err != nil {
			return err
		}
		if inside.DailyLimit == nil || *inside.DailyLimit != 900 {
			t.Errorf("a transaction does not see its own write: %+v", inside)
		}

		return nil
	})
	must(t, err)
	if w, _ := s.WalletByID(ctx, f.wallet.ID); w.DailyLimit == nil || *w.DailyLimit != 900 {
		t.Errorf("a committed write is missing: %+v", w)
	}

	// An error rolls everything back, whatever was written before it.
	boom := errors.New("boom")
	err = s.Do(ctx, func(ctx context.Context, r application.Repository) error {
		w, _ := r.WalletByID(ctx, f.wallet.ID)
		w.DailyLimit = nil
		if err := r.SaveWallet(ctx, w); err != nil {
			return err
		}
		if err := r.InsertOperation(ctx, op(1, f.a.ID)); err != nil {
			return err
		}
		if _, err := r.EnsureWallet(ctx, domain.Wallet{ID: testsupport.UUID(77), UserID: testsupport.UUID(88), CreatedAt: base, UpdatedAt: base}); err != nil {
			return err
		}

		return boom
	})
	is(t, err, boom)

	if w, _ := s.WalletByID(ctx, f.wallet.ID); w.DailyLimit == nil || *w.DailyLimit != 900 {
		t.Errorf("a rolled-back update leaked: %+v", w)
	}
	if _, err := s.OperationByID(ctx, op(1, f.a.ID).ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a rolled-back insert leaked: %v", err)
	}
	if _, err := s.WalletByUser(ctx, testsupport.UUID(88)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a rolled-back wallet leaked: %v", err)
	}
}
