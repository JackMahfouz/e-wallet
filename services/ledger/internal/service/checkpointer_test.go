package service_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/service"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/testdb"
)

// clock is a settable time source for the checkpointer.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (f fixture) checkpoints(t *testing.T, account string, period domain.Period) []domain.Checkpoint {
	t.Helper()

	checkpoints, err := f.ledger.ListCheckpoints(ctx, account, period, 1000)
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}

	return checkpoints
}

func TestCheckpointerClosesEveryFinishedPeriodAndNoOther(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	testdb.InsertEntryAt(t, f.pool, account, domain.Credit, 10000, at(2025, 1, 10, 0))
	testdb.InsertEntryAt(t, f.pool, account, domain.Credit, 500, at(2026, 3, 3, 0))
	now := &clock{now: at(2026, 11, 9, 12)}
	checkpointer := service.NewCheckpointer(f.store, time.Minute, now.Now, discardLogger())

	run := func(label string, want int) {
		t.Helper()

		written, err := checkpointer.RunOnce(ctx)
		if err != nil || written != want {
			t.Fatalf("%s: wrote %d checkpoints, err = %v; want %d", label, written, err, want)
		}
	}
	perPeriod := func() map[domain.Period]int {
		counts := map[domain.Period]int{}
		for _, period := range domain.AllPeriods {
			counts[period] = len(f.checkpoints(t, account, period))
		}

		return counts
	}

	// 9 Nov 2026: January 2025 and March 2026 are over, as are Q1 of both years and 2025.
	// 2026 and the 2025-2029 block are still open.
	run("first run", 2+2+1+0)
	if got := perPeriod(); got[domain.PeriodMonth] != 2 || got[domain.PeriodQuarter] != 2 || got[domain.PeriodYear] != 1 || got[domain.PeriodFiveYear] != 0 {
		t.Errorf("checkpoints per period = %v", got)
	}

	run("running again changes nothing", 0)

	now.Set(at(2027, 2, 1, 0))
	run("once 2026 has ended", 1)

	now.Set(at(2030, 2, 1, 0))
	run("once the 2025-2029 block has ended", 1)

	run("and again", 0)
}

func TestCheckpointerWaitsForTheGracePeriodAfterAPeriodEnds(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	testdb.InsertEntryAt(t, f.pool, account, domain.Credit, 100, at(2025, 1, 10, 0))
	now := &clock{now: at(2025, 2, 1, 0).Add(30 * time.Second)}
	checkpointer := service.NewCheckpointer(f.store, time.Minute, now.Now, discardLogger())

	early, err := checkpointer.RunOnce(ctx)
	now.Set(at(2025, 2, 1, 0).Add(2 * time.Minute))
	later, laterErr := checkpointer.RunOnce(ctx)

	if err != nil || early != 0 {
		t.Errorf("30s after the month ended: wrote %d, err = %v; want 0 (inside the 1m grace)", early, err)
	}
	if laterErr != nil || later != 1 {
		t.Errorf("2m after the month ended: wrote %d, err = %v; want 1", later, laterErr)
	}
}

func TestCheckpointsKeepTheBalanceTheSameAndTheDebitCheckHonest(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	testdb.InsertEntryAt(t, f.pool, account, domain.Credit, 10000, at(2025, 1, 10, 0))
	testdb.InsertEntryAt(t, f.pool, account, domain.Debit, 2500, at(2025, 2, 11, 0))
	testdb.InsertEntryAt(t, f.pool, account, domain.Credit, 300, at(2025, 3, 12, 0))
	before := f.balance(t, account)

	checkpointer := service.NewCheckpointer(f.store, time.Minute, func() time.Time { return at(2030, 1, 1, 0) }, discardLogger())
	written, err := checkpointer.RunOnce(ctx)
	if err != nil || written == 0 {
		t.Fatalf("RunOnce wrote %d, err = %v", written, err)
	}

	if after := f.balance(t, account); before != 7800 || after != 7800 {
		t.Errorf("balance before = %d, after checkpoints = %d; want 7800 both times", before, after)
	}
	if _, err := f.debit(account, 7801); err == nil {
		t.Error("a debit above the checkpoint-based balance was accepted")
	}
	if result, err := f.debit(account, 7800); err != nil || result.Balance != 0 {
		t.Errorf("debit of the whole balance = %+v, %v; want it accepted, leaving 0", result, err)
	}
	if _, err := f.debit(account, 1); err == nil {
		t.Error("a debit was accepted on a zero balance")
	}
}

func TestBalanceAlwaysEqualsTheSumOfTheEntriesWhateverCheckpointsExist(t *testing.T) {
	// A long random history over several accounts, with checkpoints taken at random moments (and
	// some entries stamped out of order). After every step each balance must equal a plain sum of
	// the entries, and every checkpoint must equal the entries up to its own last_seq.
	f := newFixture(t)
	random := rand.New(rand.NewPCG(11, 12))
	now := &clock{}
	checkpointer := service.NewCheckpointer(f.store, 0, now.Now, discardLogger())

	accounts := make([]string, 5)
	running := make(map[string]int64)
	for i := range accounts {
		accounts[i] = testdb.UUID()
	}

	stamp := at(2024, 1, 1, 0)
	for step := 0; step < 80; step++ {
		stamp = stamp.Add(time.Duration(1+random.IntN(45*24)) * time.Hour)

		for _, account := range accounts {
			for n := random.IntN(3); n > 0; n-- {
				at := stamp.Add(time.Duration(random.IntN(48)) * time.Hour)
				if random.IntN(10) == 0 {
					at = at.Add(-72 * time.Hour) // the clock stepped back
				}

				if running[account] > 0 && random.IntN(2) == 0 {
					cents := 1 + random.Int64N(running[account])
					testdb.InsertEntryAt(t, f.pool, account, domain.Debit, cents, at)
					running[account] -= cents
				} else {
					cents := 1 + random.Int64N(5000)
					testdb.InsertEntryAt(t, f.pool, account, domain.Credit, cents, at)
					running[account] += cents
				}
			}
		}

		if random.IntN(100) < 40 {
			now.Set(stamp.Add(time.Duration(random.IntN(150*24)) * time.Hour))
			if _, err := checkpointer.RunOnce(ctx); err != nil {
				t.Fatalf("step %d: RunOnce: %v", step, err)
			}
		}

		for _, account := range accounts {
			if got := f.balance(t, account); got != running[account] || got != testdb.BalanceThroughSeq(t, f.pool, account, 0) {
				t.Fatalf("step %d: balance = %d, running total = %d, summed entries = %d", step, got, running[account], testdb.BalanceThroughSeq(t, f.pool, account, 0))
			}
			for _, checkpoint := range f.checkpoints(t, account, "") {
				if sum := testdb.BalanceThroughSeq(t, f.pool, account, checkpoint.LastSeq); sum != checkpoint.Balance {
					t.Fatalf("step %d: %s checkpoint %v..%v has balance %d but the entries up to seq %d add up to %d",
						step, checkpoint.Period, checkpoint.PeriodStart, checkpoint.PeriodEnd, checkpoint.Balance, checkpoint.LastSeq, sum)
				}
			}
		}
	}

	now.Set(at(2040, 1, 1, 0))
	if _, err := checkpointer.RunOnce(ctx); err != nil {
		t.Fatalf("final RunOnce: %v", err)
	}
	for _, account := range accounts {
		if f.balance(t, account) != running[account] {
			t.Errorf("after closing every period the balance of %s is %d, want %d", account, f.balance(t, account), running[account])
		}
	}
}

// scriptedRepo is a Repository whose checkpoint methods are scripted by the test.
type scriptedRepo struct {
	domain.Repository

	pending func(period domain.Period) ([]string, error)
	next    func(account string, period domain.Period) (bool, error)
}

func (r *scriptedRepo) AccountsPendingCheckpoint(_ context.Context, period domain.Period) ([]string, error) {
	return r.pending(period)
}

func (r *scriptedRepo) AppendNextCheckpoint(_ context.Context, account string, period domain.Period, _ time.Time) (bool, error) {
	return r.next(account, period)
}

func TestOneFailingAccountDoesNotStopTheRest(t *testing.T) {
	broken := errors.New("disk on fire")
	remaining := map[string]int{"a": 2, "b": 5, "c": 1}
	repo := &scriptedRepo{
		pending: func(period domain.Period) ([]string, error) {
			if period == domain.PeriodMonth {
				return []string{"a", "b", "c"}, nil
			}

			return nil, nil
		},
		next: func(account string, _ domain.Period) (bool, error) {
			if account == "b" {
				return false, broken
			}
			if remaining[account] == 0 {
				return false, nil
			}
			remaining[account]--

			return true, nil
		},
	}
	checkpointer := service.NewCheckpointer(repo, 0, time.Now, discardLogger())

	written, err := checkpointer.RunOnce(ctx)

	if written != 3 {
		t.Errorf("wrote %d, want 3 (a: 2 and c: 1; b failed)", written)
	}
	if !errors.Is(err, broken) {
		t.Errorf("err = %v, want it to include the failure", err)
	}
}

func TestAFailingListingDoesNotStopTheOtherPeriods(t *testing.T) {
	listing := errors.New("cannot list")
	var asked []domain.Period
	repo := &scriptedRepo{
		pending: func(period domain.Period) ([]string, error) {
			asked = append(asked, period)
			if period == domain.PeriodQuarter {
				return nil, listing
			}

			return nil, nil
		},
		next: func(string, domain.Period) (bool, error) { return false, nil },
	}

	_, err := service.NewCheckpointer(repo, 0, time.Now, discardLogger()).RunOnce(ctx)

	if len(asked) != len(domain.AllPeriods) {
		t.Errorf("listed pending accounts for %v, want every period", asked)
	}
	if !errors.Is(err, listing) {
		t.Errorf("err = %v, want it to include the listing failure", err)
	}
}

func TestCheckpointerStopsWhenItsContextIsCancelled(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(ctx)
	calls := 0
	repo := &scriptedRepo{
		pending: func(domain.Period) ([]string, error) { return []string{"a"}, nil },
		next: func(string, domain.Period) (bool, error) {
			calls++
			cancel()

			return true, nil // would owe more periods forever
		},
	}

	_, err := service.NewCheckpointer(repo, 0, time.Now, discardLogger()).RunOnce(cancelCtx)

	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Errorf("err = %v after %d calls; want it to stop at once with context.Canceled", err, calls)
	}
}

func TestRunChecksAtOnceAndOnEveryTickUntilCancelled(t *testing.T) {
	var (
		mu    sync.Mutex
		runs  int
		first = make(chan struct{})
	)
	repo := &scriptedRepo{
		pending: func(period domain.Period) ([]string, error) {
			if period == domain.PeriodMonth { // once per RunOnce
				mu.Lock()
				runs++
				if runs == 1 {
					close(first)
				}
				mu.Unlock()
			}

			return nil, nil
		},
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		service.NewCheckpointer(repo, 0, time.Now, discardLogger()).Run(runCtx, 20*time.Millisecond)
		close(done)
	}()

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not check immediately")
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}

	mu.Lock()
	defer mu.Unlock()
	if runs < 3 {
		t.Errorf("ran %d times in about 150ms at a 20ms interval, want at least 3", runs)
	}
}
