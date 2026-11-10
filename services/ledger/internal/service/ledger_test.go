package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/service"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/store"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ctx = context.Background()

type fixture struct {
	ledger *service.Ledger
	store  *store.Store
	pool   *pgxpool.Pool
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	repo := store.New(pool)

	return fixture{ledger: service.New(repo), store: repo, pool: pool}
}

func at(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (f fixture) credit(t *testing.T, account string, cents int64) service.PostResult {
	t.Helper()

	result, err := f.ledger.Credit(ctx, service.PostInput{AccountID: account, TransactionID: testdb.UUID(), Amount: cents})
	if err != nil {
		t.Fatalf("Credit(%d): %v", cents, err)
	}

	return result
}

func (f fixture) debit(account string, cents int64) (service.PostResult, error) {
	return f.ledger.Debit(ctx, service.PostInput{AccountID: account, TransactionID: testdb.UUID(), Amount: cents})
}

func (f fixture) balance(t *testing.T, account string) int64 {
	t.Helper()

	balance, err := f.ledger.Balance(ctx, account)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}

	return balance.Balance
}

func (f fixture) entries(t *testing.T, account string) []domain.Entry {
	t.Helper()

	entries, _, err := f.ledger.ListEntries(ctx, account, 0, 1000)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}

	return entries
}

func insufficient(t *testing.T, err error) *domain.InsufficientFundsError {
	t.Helper()

	var target *domain.InsufficientFundsError
	if !errors.As(err, &target) {
		t.Fatalf("err = %v, want *InsufficientFundsError", err)
	}

	return target
}

func TestDebitsAndCreditsAppendEntriesAndTrackTheBalance(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()

	credited := f.credit(t, account, 10000)
	debited, err := f.debit(account, 4000)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}
	emptied, err := f.debit(account, 6000)
	if err != nil {
		t.Fatalf("a debit equal to the whole balance was refused: %v", err)
	}

	if credited.Balance != 10000 || debited.Balance != 6000 || emptied.Balance != 0 {
		t.Errorf("balances after each post = %d, %d, %d; want 10000, 6000, 0", credited.Balance, debited.Balance, emptied.Balance)
	}
	entries := f.entries(t, account)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	wantDirections := []domain.Direction{domain.Credit, domain.Debit, domain.Debit}
	wantAmounts := []int64{10000, 4000, 6000}
	for i, entry := range entries {
		if entry.Seq != int64(i+1) || entry.Direction != wantDirections[i] || entry.Amount != wantAmounts[i] || entry.AccountID != account {
			t.Errorf("entry %d = %+v", i+1, entry)
		}
	}
	if got := f.balance(t, account); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
}

func TestNoDebitIsAllowedOnceTheBalanceIsZero(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	f.credit(t, account, 500)
	if _, err := f.debit(account, 500); err != nil {
		t.Fatalf("emptying debit: %v", err)
	}
	entriesBefore := len(f.entries(t, account))

	_, err := f.debit(account, 1)

	if got := insufficient(t, err); got.Balance != 0 || got.Requested != 1 {
		t.Errorf("error = %+v, want balance 0 and requested 1", got)
	}
	if len(f.entries(t, account)) != entriesBefore {
		t.Error("a refused debit still appended an entry")
	}
}

func TestDebitFromAnAccountWithNoLedgerIsRefused(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()

	_, err := f.debit(account, 100)

	if got := insufficient(t, err); got.Balance != 0 || got.Requested != 100 {
		t.Errorf("error = %+v", got)
	}
	if n := len(f.entries(t, account)); n != 0 {
		t.Errorf("%d entries exist for an account that was never credited", n)
	}
}

func TestDebitLargerThanTheBalanceIsRefusedAndAppendsNothing(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	f.credit(t, account, 1000)

	_, err := f.debit(account, 1001)

	if got := insufficient(t, err); got.Balance != 1000 || got.Requested != 1001 {
		t.Errorf("error = %+v, want balance 1000 and requested 1001", got)
	}
	if n, balance := len(f.entries(t, account)), f.balance(t, account); n != 1 || balance != 1000 {
		t.Errorf("entries = %d, balance = %d; want the ledger untouched", n, balance)
	}
}

func TestEachAccountHasItsOwnLedgerAndBalance(t *testing.T) {
	f := newFixture(t)
	rich, poor := testdb.UUID(), testdb.UUID()
	f.credit(t, rich, 1_000_000)

	_, err := f.debit(poor, 1)

	insufficient(t, err)
	if f.balance(t, rich) != 1_000_000 || f.balance(t, poor) != 0 {
		t.Error("one account's money leaked into another")
	}
}

func TestARetriedPostIsNotAppliedTwice(t *testing.T) {
	f := newFixture(t)
	account, transaction := testdb.UUID(), testdb.UUID()
	f.credit(t, account, 5000)
	post := service.PostInput{AccountID: account, TransactionID: transaction, Amount: 1000}

	first, err := f.ledger.Debit(ctx, post)
	if err != nil {
		t.Fatalf("first Debit: %v", err)
	}
	retry, err := f.ledger.Debit(ctx, post)

	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if first.Replayed || !retry.Replayed {
		t.Errorf("replayed = %v then %v; want false then true", first.Replayed, retry.Replayed)
	}
	if retry.Entry.ID != first.Entry.ID || retry.Entry.Seq != first.Entry.Seq {
		t.Errorf("the retry returned entry %+v, want the original %+v", retry.Entry, first.Entry)
	}
	if n, balance := len(f.entries(t, account)), f.balance(t, account); n != 2 || balance != 4000 {
		t.Errorf("entries = %d, balance = %d; want the debit applied once (2 entries, 4000)", n, balance)
	}
}

func TestARetryStillSucceedsAfterTheBalanceHasRunOut(t *testing.T) {
	// The retry must be recognised before the balance is checked: the money has already moved.
	f := newFixture(t)
	account, transaction := testdb.UUID(), testdb.UUID()
	f.credit(t, account, 1000)
	post := service.PostInput{AccountID: account, TransactionID: transaction, Amount: 1000}
	if _, err := f.ledger.Debit(ctx, post); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	retry, err := f.ledger.Debit(ctx, post)

	if err != nil || !retry.Replayed || retry.Balance != 0 {
		t.Errorf("retry = %+v, %v; want a replay reporting the current balance of 0", retry, err)
	}
}

func TestReusingATransactionIDWithADifferentAmountIsRefused(t *testing.T) {
	f := newFixture(t)
	account, transaction := testdb.UUID(), testdb.UUID()
	if _, err := f.ledger.Credit(ctx, service.PostInput{AccountID: account, TransactionID: transaction, Amount: 1000}); err != nil {
		t.Fatalf("Credit: %v", err)
	}

	_, err := f.ledger.Credit(ctx, service.PostInput{AccountID: account, TransactionID: transaction, Amount: 1001})

	if !errors.Is(err, domain.ErrTransactionConflict) {
		t.Errorf("err = %v, want ErrTransactionConflict", err)
	}
	if n := len(f.entries(t, account)); n != 1 {
		t.Errorf("%d entries, want 1", n)
	}
}

func TestTheTwoLegsOfATransferShareATransactionID(t *testing.T) {
	f := newFixture(t)
	sender, receiver, transaction := testdb.UUID(), testdb.UUID(), testdb.UUID()
	f.credit(t, sender, 5000)

	if _, err := f.ledger.Debit(ctx, service.PostInput{AccountID: sender, TransactionID: transaction, Amount: 2500}); err != nil {
		t.Fatalf("debit leg: %v", err)
	}
	if _, err := f.ledger.Credit(ctx, service.PostInput{AccountID: receiver, TransactionID: transaction, Amount: 2500}); err != nil {
		t.Fatalf("credit leg: %v", err)
	}

	// The design's double-entry invariant: debits equal credits per transaction.
	var debits, credits int64
	err := f.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint,
		        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint
		 FROM ledger_entries WHERE transaction_id = $1`, transaction).Scan(&debits, &credits)
	if err != nil || debits != 2500 || credits != 2500 {
		t.Errorf("debits = %d, credits = %d, err = %v; want 2500 each", debits, credits, err)
	}
}

func TestBothDirectionsOfOneTransactionCanLandOnOneAccount(t *testing.T) {
	f := newFixture(t)
	account, transaction := testdb.UUID(), testdb.UUID()
	f.credit(t, account, 1000)

	_, debitErr := f.ledger.Debit(ctx, service.PostInput{AccountID: account, TransactionID: transaction, Amount: 400})
	_, creditErr := f.ledger.Credit(ctx, service.PostInput{AccountID: account, TransactionID: transaction, Amount: 400})

	if debitErr != nil || creditErr != nil || f.balance(t, account) != 1000 {
		t.Errorf("debit err = %v, credit err = %v, balance = %d; want both to land and net to 1000", debitErr, creditErr, f.balance(t, account))
	}
}

func TestInvalidInputIsRefusedBeforeTouchingTheLedger(t *testing.T) {
	f := newFixture(t)
	valid := testdb.UUID()

	tests := map[string]struct {
		input service.PostInput
		want  error
	}{
		"account id is not a uuid":     {service.PostInput{AccountID: "alice", TransactionID: testdb.UUID(), Amount: 100}, domain.ErrInvalidID},
		"account id is empty":          {service.PostInput{AccountID: "", TransactionID: testdb.UUID(), Amount: 100}, domain.ErrInvalidID},
		"transaction id is not a uuid": {service.PostInput{AccountID: valid, TransactionID: "t-1", Amount: 100}, domain.ErrInvalidID},
		"zero amount":                  {service.PostInput{AccountID: valid, TransactionID: testdb.UUID(), Amount: 0}, domain.ErrInvalidAmount},
		"negative amount":              {service.PostInput{AccountID: valid, TransactionID: testdb.UUID(), Amount: -100}, domain.ErrInvalidAmount},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, creditErr := f.ledger.Credit(ctx, tc.input)
			_, debitErr := f.ledger.Debit(ctx, tc.input)

			if !errors.Is(creditErr, tc.want) || !errors.Is(debitErr, tc.want) {
				t.Errorf("credit err = %v, debit err = %v; want %v", creditErr, debitErr, tc.want)
			}
		})
	}

	var rows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d rows were written for invalid input (err %v)", rows, err)
	}
}

func TestAccountIDsAreNormalisedSoCaseDoesNotSplitALedger(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	f.credit(t, account, 700)

	upper := make([]byte, len(account))
	for i := range account {
		c := account[i]
		if c >= 'a' && c <= 'f' {
			c -= 'a' - 'A'
		}
		upper[i] = c
	}
	balance, err := f.ledger.Balance(ctx, string(upper))

	if err != nil || balance.Balance != 700 {
		t.Errorf("Balance(%s) = %+v, %v; want the same ledger (700)", upper, balance, err)
	}
}

func TestConcurrentDebitsNeverSpendMoreThanTheBalance(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	f.credit(t, account, 3000)

	const attempts = 100
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		refused   int
		other     []error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.debit(account, 100)

			mu.Lock()
			defer mu.Unlock()
			var funds *domain.InsufficientFundsError
			switch {
			case err == nil:
				succeeded++
			case errors.As(err, &funds):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if succeeded != 30 || refused != 70 {
		t.Errorf("%d debits succeeded and %d were refused; want exactly 30 and 70 (30.00 funds 30 debits of 1.00)", succeeded, refused)
	}
	if got := f.balance(t, account); got != 0 {
		t.Errorf("final balance = %d, want 0", got)
	}
	assertContiguousAndNeverNegative(t, f, account, 31)
}

func TestConcurrentCreditsAllLand(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.ledger.Credit(ctx, service.PostInput{AccountID: account, TransactionID: testdb.UUID(), Amount: 250}); err != nil {
				t.Errorf("Credit: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := f.balance(t, account); got != 15000 {
		t.Errorf("balance = %d, want 15000", got)
	}
	assertContiguousAndNeverNegative(t, f, account, 60)
}

func TestConcurrentMixedPostingsNeverMakeTheBalanceNegativeAtAnyPoint(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	f.credit(t, account, 500)

	random := rand.New(rand.NewPCG(7, 8))
	type operation struct {
		direction domain.Direction
		cents     int64
	}
	operations := make([]operation, 300)
	for i := range operations {
		operations[i] = operation{domain.Credit, 1 + random.Int64N(400)}
		if random.IntN(2) == 0 {
			operations[i].direction = domain.Debit
			operations[i].cents = 1 + random.Int64N(600)
		}
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	jobs := make(chan operation)
	for worker := 0; worker < 24; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for op := range jobs {
				input := service.PostInput{AccountID: account, TransactionID: testdb.UUID(), Amount: op.cents}
				var err error
				if op.direction == domain.Debit {
					_, err = f.ledger.Debit(ctx, input)
				} else {
					_, err = f.ledger.Credit(ctx, input)
				}

				var funds *domain.InsufficientFundsError
				if err != nil && !errors.As(err, &funds) {
					t.Errorf("unexpected error: %v", err)

					continue
				}
				if err == nil {
					mu.Lock()
					succeeded++
					mu.Unlock()
				}
			}
		}()
	}
	for _, op := range operations {
		jobs <- op
	}
	close(jobs)
	wg.Wait()

	// Replay the ledger in order: the balance must never have dipped below zero, and every
	// successful post must be exactly one entry.
	assertContiguousAndNeverNegative(t, f, account, int64(1+succeeded))
}

// assertContiguousAndNeverNegative replays an account's entries in sequence order and checks the
// ledger's core invariants: no gaps in the sequence, a running balance that is never negative,
// and a final balance equal to the service's answer and to a plain sum of the entries.
func assertContiguousAndNeverNegative(t *testing.T, f fixture, account string, wantEntries int64) {
	t.Helper()

	entries := f.entries(t, account)
	if int64(len(entries)) != wantEntries {
		t.Errorf("%d entries, want %d", len(entries), wantEntries)
	}

	var running int64
	for i, entry := range entries {
		if entry.Seq != int64(i+1) {
			t.Fatalf("entry %d has seq %d: the sequence has a gap or a duplicate", i+1, entry.Seq)
		}
		running += entry.Direction.Sign() * entry.Amount
		if running < 0 {
			t.Fatalf("the balance was %d after seq %d: an overdraft got through", running, entry.Seq)
		}
	}

	if got := f.balance(t, account); got != running || got != testdb.BalanceThroughSeq(t, f.pool, account, 0) {
		t.Errorf("Balance = %d, replayed = %d, summed = %d; want all three equal", got, running, testdb.BalanceThroughSeq(t, f.pool, account, 0))
	}
}

func TestListEntriesPagesWithHasMore(t *testing.T) {
	f := newFixture(t)
	account := testdb.UUID()
	for i := 0; i < 5; i++ {
		f.credit(t, account, int64(100+i))
	}

	page := func(afterSeq int64, limit int) ([]int64, bool) {
		entries, more, err := f.ledger.ListEntries(ctx, account, afterSeq, limit)
		if err != nil {
			t.Fatalf("ListEntries: %v", err)
		}
		var seqs []int64
		for _, e := range entries {
			seqs = append(seqs, e.Seq)
		}

		return seqs, more
	}

	tests := map[string]struct {
		afterSeq int64
		limit    int
		seqs     []int64
		more     bool
	}{
		"first page":                       {0, 2, []int64{1, 2}, true},
		"middle page":                      {2, 2, []int64{3, 4}, true},
		"last page":                        {4, 2, []int64{5}, false},
		"a page that ends exactly":         {0, 5, []int64{1, 2, 3, 4, 5}, false},
		"a limit of zero uses the default": {0, 0, []int64{1, 2, 3, 4, 5}, false},
		"a negative position is zero":      {-3, 10, []int64{1, 2, 3, 4, 5}, false},
		"past the end":                     {9, 10, nil, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			seqs, more := page(tc.afterSeq, tc.limit)

			if more != tc.more || len(seqs) != len(tc.seqs) {
				t.Fatalf("got %v (more=%v), want %v (more=%v)", seqs, more, tc.seqs, tc.more)
			}
			for i := range seqs {
				if seqs[i] != tc.seqs[i] {
					t.Fatalf("got %v, want %v", seqs, tc.seqs)
				}
			}
		})
	}
}

func TestReadsRejectABadAccountID(t *testing.T) {
	f := newFixture(t)

	_, balanceErr := f.ledger.Balance(ctx, "nope")
	_, _, entriesErr := f.ledger.ListEntries(ctx, "nope", 0, 10)
	_, checkpointsErr := f.ledger.ListCheckpoints(ctx, "nope", "", 10)

	for name, err := range map[string]error{"Balance": balanceErr, "ListEntries": entriesErr, "ListCheckpoints": checkpointsErr} {
		if !errors.Is(err, domain.ErrInvalidID) {
			t.Errorf("%s err = %v, want ErrInvalidID", name, err)
		}
	}
}

func TestListCheckpointsRejectsAnUnknownPeriod(t *testing.T) {
	f := newFixture(t)

	_, err := f.ledger.ListCheckpoints(ctx, testdb.UUID(), "week", 10)

	if !errors.Is(err, domain.ErrInvalidPeriod) {
		t.Errorf("err = %v, want ErrInvalidPeriod", err)
	}
}

// fakeRepo records the limits the service asks for.
type fakeRepo struct {
	domain.Repository
	entriesLimit     int
	checkpointsLimit int
}

func (f *fakeRepo) ListEntries(_ context.Context, _ string, _ int64, limit int) ([]domain.Entry, error) {
	f.entriesLimit = limit

	return nil, nil
}

func (f *fakeRepo) ListCheckpoints(_ context.Context, _ string, _ domain.Period, limit int) ([]domain.Checkpoint, error) {
	f.checkpointsLimit = limit

	return nil, nil
}

func TestListLimitsAreClamped(t *testing.T) {
	repo := &fakeRepo{}
	ledger := service.New(repo)
	account := testdb.UUID()

	tests := map[int]int{0: 100, -5: 100, 1: 1, 50: 50, 1000: 1000, 5000: 1000}
	for asked, want := range tests {
		if _, _, err := ledger.ListEntries(ctx, account, 0, asked); err != nil {
			t.Fatal(err)
		}
		// ListEntries asks the repository for one extra row, to learn whether a page follows.
		if repo.entriesLimit != want+1 {
			t.Errorf("ListEntries(limit %d) asked the repository for %d rows, want %d", asked, repo.entriesLimit, want+1)
		}

		if _, err := ledger.ListCheckpoints(ctx, account, "", asked); err != nil {
			t.Fatal(err)
		}
		if repo.checkpointsLimit != want {
			t.Errorf("ListCheckpoints(limit %d) asked the repository for %d rows, want %d", asked, repo.checkpointsLimit, want)
		}
	}
}
