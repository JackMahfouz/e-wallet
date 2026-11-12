package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

// This is the whole core running on real PostgreSQL transactions and row locks, against a stateful
// fake ledger: what the in-memory store can only approximate by serialising everything.

type realWorld struct {
	svc    *application.Service
	ledger *testsupport.Ledger
	clock  *testsupport.Clock
}

func newRealWorld(t *testing.T, mutate ...func(*application.Options)) *realWorld {
	t.Helper()

	return newRealWorldWithLedger(t, func(l application.Ledger) application.Ledger { return l }, mutate...)
}

// newRealWorldWithLedger wraps the fake ledger, for tests that need to hold a call in flight.
func newRealWorldWithLedger(t *testing.T, wrap func(application.Ledger) application.Ledger, mutate ...func(*application.Options)) *realWorld {
	t.Helper()

	store, _ := newStore(t)
	w := &realWorld{ledger: &testsupport.Ledger{}, clock: testsupport.NewClock()}

	opts := application.DefaultOptions()
	opts.FundingEnabled = true
	for _, change := range mutate {
		change(&opts)
	}
	w.svc = application.New(application.Dependencies{
		Store: store, Ledger: wrap(w.ledger), Tokens: &testsupport.Tokens{}, OTP: testsupport.OTP{},
		Funding: &testsupport.Funding{}, Clock: w.clock, IDs: &testsupport.SeqIDs{},
		Logger: slog.New(slog.DiscardHandler),
	}, opts)

	return w
}

func (w *realWorld) account(t *testing.T, user, name string, balance int64) string {
	t.Helper()

	view, err := w.svc.CreateAccount(ctx, user, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.ledger.Fund(view.ID, balance)

	return view.ID
}

func key(n int) string { return testsupport.UUID(100_000 + n) }

func TestConcurrentWithdrawalsOnRealTransactionsNeverOverspend(t *testing.T) {
	w := newRealWorld(t)
	user := testsupport.UUID(1)
	account := w.account(t, user, "a", 100)

	const attempts = 40
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		completed int
		refused   int
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			op, err := w.svc.Withdraw(ctx, application.UserMoneyRequest{
				UserID: user, AccountID: account, Amount: 10, IdempotencyKey: key(i), OTP: testsupport.ValidOTP,
			})

			mu.Lock()
			defer mu.Unlock()
			var insufficient *domain.InsufficientFundsError
			switch {
			case err == nil && op.Status == domain.StatusCompleted:
				completed++
			case errors.As(err, &insufficient):
				refused++
			default:
				t.Errorf("unexpected: %v, %+v", err, op)
			}
		}()
	}
	wg.Wait()

	if completed != 10 || refused != attempts-10 {
		t.Errorf("completed = %d, refused = %d; want 10 and %d", completed, refused, attempts-10)
	}
	if got := w.ledger.BalanceOf(account); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
}

func TestConcurrentSameKeyRequestsMakeOneOperation(t *testing.T) {
	w := newRealWorld(t)
	user := testsupport.UUID(1)
	account := w.account(t, user, "a", 1000)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]bool{}
	)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			op, err := w.svc.Withdraw(ctx, application.UserMoneyRequest{
				UserID: user, AccountID: account, Amount: 100, IdempotencyKey: key(1), OTP: testsupport.ValidOTP,
			})
			if err != nil {
				t.Errorf("Withdraw: %v", err)

				return
			}

			mu.Lock()
			ids[op.ID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(ids) != 1 {
		t.Errorf("the same key made %d operations", len(ids))
	}
	if got := w.ledger.BalanceOf(account); got != 900 {
		t.Errorf("balance = %d, want 900 (one withdrawal)", got)
	}
}

func TestConcurrentTransfersWithFlakyLedgerConserveMoneyAndAllFinish(t *testing.T) {
	w := newRealWorld(t)
	ada, bob := testsupport.UUID(1), testsupport.UUID(2)
	accounts := []string{
		w.account(t, ada, "a1", 600), w.account(t, ada, "a2", 600),
		w.account(t, bob, "b1", 600), w.account(t, bob, "b2", 600),
	}
	owner := map[string]string{accounts[0]: ada, accounts[1]: ada, accounts[2]: bob, accounts[3]: bob}
	const total = 2400

	var faultMu sync.Mutex
	rng := rand.New(rand.NewPCG(3, 4))
	w.ledger.Fault = func(testsupport.LedgerCall) testsupport.Fault {
		faultMu.Lock()
		defer faultMu.Unlock()

		switch rng.IntN(10) {
		case 0, 1:
			return testsupport.FaultUnavailable
		case 2:
			return testsupport.FaultLostResponse
		default:
			return testsupport.FaultNone
		}
	}

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids []string
	)
	for i := 0; i < 60; i++ {
		from, to := accounts[i%4], accounts[(i*3+1)%4]
		if from == to {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			op, err := w.svc.Transfer(ctx, application.TransferRequest{
				UserID: owner[from], FromAccountID: from, ToAccountID: to, Amount: int64(10 + i%200),
				IdempotencyKey: key(1000 + i), OTP: testsupport.ValidOTP,
			})
			if err != nil {
				return // refused cleanly (insufficient funds, or the ledger down before anything was recorded)
			}

			mu.Lock()
			ids = append(ids, op.ID)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// The faults stop, and recovery finishes whatever was left half-way.
	w.ledger.Fault = nil
	for round := 0; round < 30; round++ {
		w.clock.Advance(time.Hour)
		if _, err := w.svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range ids {
		op, err := w.svc.OperationFor(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !op.Status.Terminal() {
			t.Errorf("operation %s is still %s", id, op.Status)
		}
	}
	if got := w.ledger.Total(); got != total {
		t.Errorf("total money = %d, want %d", got, total)
	}
	for _, id := range accounts {
		if got := w.ledger.BalanceOf(id); got < 0 {
			t.Errorf("account %s overdrawn: %d", id, got)
		}
	}
}

func TestTwoRecoveryWorkersNeverAdvanceTheSameOperationTwice(t *testing.T) {
	w := newRealWorld(t)
	ada, bob := testsupport.UUID(1), testsupport.UUID(2)
	from, to := w.account(t, ada, "a", 5000), w.account(t, bob, "b", 0)

	w.ledger.Fault = func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == "Debit" {
			return testsupport.FaultUnavailable
		}

		return testsupport.FaultNone
	}
	for i := 0; i < 20; i++ {
		if _, err := w.svc.Transfer(ctx, application.TransferRequest{
			UserID: ada, FromAccountID: from, ToAccountID: to, Amount: 10, IdempotencyKey: key(2000 + i), OTP: testsupport.ValidOTP,
		}); err != nil {
			t.Fatal(err)
		}
	}

	w.ledger.Fault = nil
	w.clock.Advance(time.Hour)
	debitsBefore := w.ledger.CallCount("Debit") // the 20 first attempts, which the fault refused

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		advanced int
	)
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			report, err := w.svc.Recover(ctx)
			if err != nil {
				t.Errorf("Recover: %v", err)

				return
			}

			mu.Lock()
			advanced += report.Advanced
			mu.Unlock()
		}()
	}
	wg.Wait()

	if advanced != 20 {
		t.Errorf("workers advanced %d operations between them, want exactly 20", advanced)
	}
	if got := w.ledger.BalanceOf(to); got != 200 {
		t.Errorf("destination balance = %d, want 200", got)
	}
	if n := w.ledger.CallCount("Debit") - debitsBefore; n != 20 {
		t.Errorf("ledger debits during recovery = %d, want 20: each operation is driven by one worker", n)
	}
}

// The account lock serialises spending from one account. What stops two different accounts of one
// wallet from each spending within the wallet's limit, and together going over it, is the wallet
// lock. Without it, both would read the same "spent so far" and both be let through.
func TestConcurrentSpendingFromDifferentAccountsNeverExceedsTheWalletLimit(t *testing.T) {
	const accountsCount = 30

	w := newRealWorld(t, func(o *application.Options) { o.MaxAccounts = accountsCount })
	user := testsupport.UUID(1)

	accounts := make([]string, accountsCount)
	for i := range accounts {
		accounts[i] = w.account(t, user, "acct", 100_000) // 1000.00 each, so funds never matter
	}

	// A wallet limit of 100.00 and withdrawals of 30.00 from thirty accounts at once: exactly three fit.
	limit := int64(10_000)
	if _, err := w.svc.SetWalletLimit(ctx, user, testsupport.ValidOTP, application.LimitChange{Set: true, Value: &limit}); err != nil {
		t.Fatal(err)
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		completed int
		waiting   int
	)
	start := make(chan struct{})

	for i, account := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			op, err := w.svc.Withdraw(ctx, application.UserMoneyRequest{
				UserID: user, AccountID: account, Amount: 3_000, IdempotencyKey: key(3000 + i), OTP: testsupport.ValidOTP,
			})

			mu.Lock()
			defer mu.Unlock()
			var confirmation *domain.ConfirmationRequiredError
			switch {
			case err == nil && op.Status == domain.StatusCompleted:
				completed++
			case errors.As(err, &confirmation):
				waiting++
			default:
				t.Errorf("unexpected: %v, %+v", err, op)
			}
		}()
	}
	close(start)
	wg.Wait()

	if completed != 3 || waiting != accountsCount-3 {
		t.Errorf("completed = %d, awaiting confirmation = %d; want 3 and %d: the wallet limit was %s",
			completed, waiting, accountsCount-3, "100.00")
	}

	var spent int64
	for _, account := range accounts {
		spent += 100_000 - w.ledger.BalanceOf(account)
	}
	if spent != 9_000 {
		t.Errorf("spent %d cents, want 9000 (three withdrawals of 30.00)", spent)
	}
}

// gatedLedger holds the first debit in flight: the ledger has applied it, and the caller has not yet
// been told.
type gatedLedger struct {
	application.Ledger
	armed   atomic.Bool
	applied chan struct{} // closed once the held debit has reached the ledger
	release chan struct{} // closed by Release to let it return
	once    sync.Once
}

// Release lets the held debit return. It is safe to call more than once.
func (g *gatedLedger) Release() { g.once.Do(func() { close(g.release) }) }

func (g *gatedLedger) Debit(ctx context.Context, account, transaction string, cents int64) (application.LedgerPosting, error) {
	posting, err := g.Ledger.Debit(ctx, account, transaction, cents)
	if err == nil && g.armed.CompareAndSwap(true, false) {
		close(g.applied)
		<-g.release
	}

	return posting, err
}

// While a debit is between "the ledger applied it" and "the operation says so", the money is
// already gone from the balance and still counted as a pending operation. Another request that
// worked out what the account can spend in that instant would count it twice and wrongly refuse.
// The debit holds the account's lock for exactly that reason, so the other request must wait.
func TestADebitInFlightMakesTheNextRequestOnTheSameAccountWaitInsteadOfMiscounting(t *testing.T) {
	var gate *gatedLedger

	w := newRealWorldWithLedger(t, func(l application.Ledger) application.Ledger {
		gate = &gatedLedger{Ledger: l, applied: make(chan struct{}), release: make(chan struct{})}

		return gate
	})
	// If this test fails while a debit is held, let it go, or the pool's cleanup would wait for the
	// connection that debit is holding and the failure would only show up as a timeout.
	t.Cleanup(gate.Release)

	user := testsupport.UUID(1)
	account := w.account(t, user, "a", 10_000) // 100.00

	type result struct {
		op  domain.Operation
		err error
	}
	withdraw := func(amount int64, k int) <-chan result {
		out := make(chan result, 1)
		go func() {
			op, err := w.svc.Withdraw(ctx, application.UserMoneyRequest{
				UserID: user, AccountID: account, Amount: amount, IdempotencyKey: key(4000 + k), OTP: testsupport.ValidOTP,
			})
			out <- result{op, err}
		}()

		return out
	}

	gate.armed.Store(true)
	first := withdraw(6_000, 1) // 60.00: its debit is applied, then held in flight

	select {
	case <-gate.applied:
	case <-time.After(10 * time.Second):
		t.Fatal("the first debit never reached the ledger")
	}

	// 40.00 is exactly what is left. Counting the first withdrawal twice would make it look like 0.
	second := withdraw(4_000, 2)

	select {
	case r := <-second:
		t.Fatalf("the second request did not wait for the debit in flight: %v, %+v", r.err, r.op)
	case <-time.After(500 * time.Millisecond):
	}

	gate.Release()

	for name, ch := range map[string]<-chan result{"first": first, "second": second} {
		select {
		case r := <-ch:
			if r.err != nil || r.op.Status != domain.StatusCompleted {
				t.Errorf("%s withdrawal: %v, %+v; want it completed", name, r.err, r.op)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s withdrawal never finished", name)
		}
	}

	if got := w.ledger.BalanceOf(account); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
}
