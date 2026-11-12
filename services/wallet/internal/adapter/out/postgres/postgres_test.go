package postgres_test

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/postgres"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport/pgtest"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport/storecontract"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ctx  = context.Background()
	base = time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
)

func newStore(t testing.TB) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()

	pool := pgtest.New(t)

	return postgres.New(pool), pool
}

func TestPostgresMeetsTheStoreContract(t *testing.T) {
	storecontract.Run(t, func(t *testing.T) application.Store {
		store, _ := newStore(t)

		return store
	})
}

func TestMigrationsCanBeAppliedTwice(t *testing.T) {
	_, pool := newStore(t)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

// checkCodes pulls the numbers out of a CHECK constraint such as CHECK ((status = ANY (ARRAY[1, 2, 3]))).
func checkCodes(t *testing.T, pool *pgxpool.Pool, constraint string) []int {
	t.Helper()

	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1 AND connamespace = current_schema()::regnamespace`,
		constraint).Scan(&def); err != nil {
		t.Fatalf("constraint %s: %v", constraint, err)
	}

	var codes []int
	for _, m := range regexp.MustCompile(`\d+`).FindAllString(def, -1) {
		n, _ := strconv.Atoi(m)
		codes = append(codes, n)
	}
	sort.Ints(codes)

	return codes
}

func TestEnumCodesAgreeWithTheCheckConstraints(t *testing.T) {
	_, pool := newStore(t)

	toInts := func(n int, code func(int) int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = code(i)
		}

		return out
	}

	tests := map[string]struct {
		constraint string
		want       []int
	}{
		"account status":   {"accounts_status_check", toInts(len(domain.AccountStatuses), func(i int) int { return int(domain.AccountStatuses[i]) })},
		"operation kind":   {"operations_kind_check", toInts(len(domain.OperationKinds), func(i int) int { return int(domain.OperationKinds[i]) })},
		"operation status": {"operations_status_check", toInts(len(domain.OperationStatuses), func(i int) int { return int(domain.OperationStatuses[i]) })},
		"failure reason":   {"operations_failure_reason_check", toInts(len(domain.FailureReasons), func(i int) int { return int(domain.FailureReasons[i]) })},
		"limit scope":      {"operations_limit_scope_check", toInts(len(domain.LimitScopes), func(i int) int { return int(domain.LimitScopes[i]) })},
	}

	for name, tc := range tests {
		got := checkCodes(t, pool, tc.constraint)
		sort.Ints(tc.want)

		if len(got) != len(tc.want) {
			t.Errorf("%s: the database allows %v, the domain defines %v", name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: the database allows %v, the domain defines %v", name, got, tc.want)

				break
			}
		}
	}
}

func seed(t testing.TB, s *postgres.Store) (walletID, a, b, c string) {
	t.Helper()

	w1 := domain.Wallet{ID: testsupport.UUID(1), UserID: testsupport.UUID(11), CreatedAt: base, UpdatedAt: base}
	w2 := domain.Wallet{ID: testsupport.UUID(2), UserID: testsupport.UUID(12), CreatedAt: base, UpdatedAt: base}
	for _, w := range []domain.Wallet{w1, w2} {
		if _, err := s.EnsureWallet(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	for i, spec := range []struct{ id, wallet string }{{testsupport.UUID(21), w1.ID}, {testsupport.UUID(22), w1.ID}, {testsupport.UUID(23), w2.ID}} {
		err := s.InsertAccount(ctx, domain.Account{
			ID: spec.id, WalletID: spec.wallet, Name: "acct", Status: domain.AccountActive,
			CreatedAt: base.Add(time.Duration(i) * time.Second), UpdatedAt: base,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	return w1.ID, testsupport.UUID(21), testsupport.UUID(22), testsupport.UUID(23)
}

func validOp(n int, account string) domain.Operation {
	return domain.Operation{
		ID: testsupport.UUID(1000 + n), Kind: domain.KindDeposit, AccountID: account, Amount: 100,
		Status: domain.StatusPending, Initiator: "user:x", IdempotencyKey: testsupport.UUID(2000 + n),
		RequestHash: "h", DeadlineAt: base.Add(time.Hour), NextAttemptAt: base, CreatedAt: base, UpdatedAt: base,
		ExecutedAt: &base,
	}
}

func TestTheDatabaseRefusesRowsTheDomainWouldNeverMake(t *testing.T) {
	s, _ := newStore(t)
	_, a, _, c := seed(t, s)

	mutate := func(n int, change func(*domain.Operation)) domain.Operation {
		op := validOp(n, a)
		change(&op)

		return op
	}

	tests := map[string]struct {
		op         domain.Operation
		constraint string
	}{
		"zero amount":                 {mutate(1, func(o *domain.Operation) { o.Amount = 0 }), "operations_amount_check"},
		"unknown kind":                {mutate(2, func(o *domain.Operation) { o.Kind = 9 }), "operations_kind_check"},
		"unknown status":              {mutate(3, func(o *domain.Operation) { o.Status = 9 }), "operations_status_check"},
		"transfer without a peer":     {mutate(4, func(o *domain.Operation) { o.Kind = domain.KindTransfer }), "operations_transfer_has_counterparty"},
		"a peer on a deposit":         {mutate(5, func(o *domain.Operation) { o.CounterpartyID = c }), "operations_transfer_has_counterparty"},
		"transfer to itself":          {mutate(6, func(o *domain.Operation) { o.Kind, o.CounterpartyID = domain.KindTransfer, a }), "operations_counterparty_differs"},
		"a reason without failing":    {mutate(7, func(o *domain.Operation) { o.Failure = domain.FailureRejected }), "operations_failure_reason"},
		"failed with no reason":       {mutate(8, func(o *domain.Operation) { o.Status, o.CompletedAt = domain.StatusFailed, &base }), "operations_failure_reason"},
		"awaiting with no scope":      {mutate(9, func(o *domain.Operation) { o.Status, o.ExecutedAt = domain.StatusAwaitingConfirmation, nil }), "operations_awaiting"},
		"pending never executed":      {mutate(10, func(o *domain.Operation) { o.ExecutedAt = nil }), "operations_executed"},
		"completed without a time":    {mutate(11, func(o *domain.Operation) { o.Status = domain.StatusCompleted }), "operations_completed"},
		"a time without completing":   {mutate(12, func(o *domain.Operation) { o.CompletedAt = &base }), "operations_completed"},
		"unknown account":             {mutate(13, func(o *domain.Operation) { o.AccountID = testsupport.UUID(900) }), "operations_account_id_fkey"},
		"unknown hold":                {mutate(14, func(o *domain.Operation) { o.HoldID = testsupport.UUID(900) }), "operations_hold_id_fkey"},
		"negative attempts":           {mutate(15, func(o *domain.Operation) { o.Attempts = -1 }), "operations_attempts_check"},
		"unknown limit scope":         {mutate(16, func(o *domain.Operation) { o.LimitScope = 9 }), "operations_limit_scope_check"},
		"unknown failure reason code": {mutate(17, func(o *domain.Operation) { o.Status, o.Failure, o.CompletedAt = domain.StatusFailed, 9, &base }), "operations_failure_reason_check"},
	}

	for name, tc := range tests {
		err := s.InsertOperation(ctx, tc.op)

		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint {
			t.Errorf("%s: err = %v, want a violation of %s", name, err, tc.constraint)
		}
	}

	// Accounts, wallets and holds too.
	closedWithoutTime := domain.Account{ID: testsupport.UUID(31), WalletID: testsupport.UUID(1), Name: "x", Status: domain.AccountClosed, CreatedAt: base, UpdatedAt: base}
	zeroLimit := int64(0)
	for name, bad := range map[string]struct {
		err        error
		constraint string
	}{
		"closed without a time":      {s.InsertAccount(ctx, closedWithoutTime), "accounts_closed_at"},
		"account limit zero":         {s.InsertAccount(ctx, domain.Account{ID: testsupport.UUID(32), WalletID: testsupport.UUID(1), Name: "x", Status: domain.AccountActive, DailyLimit: &zeroLimit, CreatedAt: base, UpdatedAt: base}), "accounts_daily_limit_check"},
		"empty name":                 {s.InsertAccount(ctx, domain.Account{ID: testsupport.UUID(33), WalletID: testsupport.UUID(1), Name: "", Status: domain.AccountActive, CreatedAt: base, UpdatedAt: base}), "accounts_name_check"},
		"unknown status":             {s.InsertAccount(ctx, domain.Account{ID: testsupport.UUID(34), WalletID: testsupport.UUID(1), Name: "x", Status: 7, CreatedAt: base, UpdatedAt: base}), "accounts_status_check"},
		"hold of zero":               {s.InsertHold(ctx, domain.Hold{ID: testsupport.UUID(41), AccountID: a, Amount: 0, ExpiresAt: base, Initiator: "service:x", IdempotencyKey: testsupport.UUID(51), RequestHash: "h", CreatedAt: base}), "holds_amount_check"},
		"hold released and captured": {s.InsertHold(ctx, domain.Hold{ID: testsupport.UUID(42), AccountID: a, Amount: 1, ExpiresAt: base, ReleasedAt: &base, CapturedBy: testsupport.UUID(900), Initiator: "service:x", IdempotencyKey: testsupport.UUID(52), RequestHash: "h", CreatedAt: base}), "holds_released_or_captured"},
	} {
		var pgErr *pgconn.PgError
		if !errors.As(bad.err, &pgErr) || pgErr.ConstraintName != bad.constraint {
			t.Errorf("%s: err = %v, want a violation of %s", name, bad.err, bad.constraint)
		}
	}

	// A user has one wallet, whatever happens to the ids.
	_, err := s.EnsureWallet(ctx, domain.Wallet{ID: testsupport.UUID(77), UserID: testsupport.UUID(11), CreatedAt: base, UpdatedAt: base})
	if err != nil {
		t.Errorf("EnsureWallet for an existing user: %v", err)
	}
	if err := s.SaveWallet(ctx, domain.Wallet{ID: testsupport.UUID(1), DailyLimit: &zeroLimit, UpdatedAt: base}); err == nil {
		t.Error("a wallet limit of zero must be refused")
	}
}

func TestOnlyOneOfManyConcurrentInsertsWithTheSameKeySucceeds(t *testing.T) {
	s, _ := newStore(t)
	_, a, _, _ := seed(t, s)

	const racers = 12
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		dupes     int
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			op := validOp(100+i, a)
			op.IdempotencyKey = testsupport.UUID(5000) // everyone uses the same key
			err := s.InsertOperation(ctx, op)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, domain.ErrDuplicateKey):
				dupes++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 || dupes != racers-1 {
		t.Errorf("succeeded = %d, duplicates = %d; want 1 and %d", succeeded, dupes, racers-1)
	}
}

func TestLockWalletMakesOtherTransactionsWait(t *testing.T) {
	s, _ := newStore(t)
	walletID, _, _, _ := seed(t, s)

	holding := make(chan struct{})
	release := make(chan struct{})
	order := make(chan string, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_ = s.Do(ctx, func(ctx context.Context, r application.Repository) error {
			if _, err := r.LockWallet(ctx, walletID); err != nil {
				return err
			}
			close(holding)
			<-release
			order <- "first"

			return nil
		})
	}()

	go func() {
		defer wg.Done()
		<-holding
		_ = s.Do(ctx, func(ctx context.Context, r application.Repository) error {
			if _, err := r.LockWallet(ctx, walletID); err != nil { // must wait for the first
				return err
			}
			order <- "second"

			return nil
		})
	}()

	<-holding
	time.Sleep(300 * time.Millisecond) // long enough for the second to be stuck on the lock
	if len(order) != 0 {
		t.Fatal("the second transaction got the wallet lock while the first still held it")
	}
	close(release)
	wg.Wait()

	if first, second := <-order, <-order; first != "first" || second != "second" {
		t.Errorf("order = %s then %s", first, second)
	}
}

func TestLockingTheSameAccountsInOppositeOrdersDoesNotDeadlock(t *testing.T) {
	s, _ := newStore(t)
	_, a, b, _ := seed(t, s)

	done := make(chan struct{})
	go func() {
		defer close(done)

		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			ids := []string{a, b}
			if i%2 == 1 {
				ids = []string{b, a} // the adapter must lock in id order whatever it is asked
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				err := s.Do(ctx, func(ctx context.Context, r application.Repository) error {
					_, err := r.LockAccounts(ctx, ids...)
					time.Sleep(2 * time.Millisecond)

					return err
				})
				if err != nil {
					t.Errorf("transaction failed: %v", err)
				}
			}()
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: transactions locking the same accounts in opposite orders never finished")
	}
}

func TestClaimDueNeverGivesTheSameOperationToTwoWorkers(t *testing.T) {
	s, _ := newStore(t)
	_, a, _, _ := seed(t, s)

	const total = 60
	for n := 1; n <= total; n++ {
		if err := s.InsertOperation(ctx, validOp(n, a)); err != nil {
			t.Fatal(err)
		}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = map[string]int{}
	)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
				ops, err := s.ClaimDue(ctx, base.Add(time.Hour), time.Hour, 4)
				if err != nil {
					t.Errorf("ClaimDue: %v", err)

					return
				}
				if len(ops) == 0 {
					return
				}

				mu.Lock()
				for _, op := range ops {
					claimed[op.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != total {
		t.Errorf("claimed %d distinct operations, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("operation %s was claimed %d times", id, n)
		}
	}
}

func TestClaimDueSkipsOperationsAnotherTransactionHasLocked(t *testing.T) {
	s, _ := newStore(t)
	_, a, _, _ := seed(t, s)

	locked, free := validOp(1, a), validOp(2, a)
	for _, op := range []domain.Operation{locked, free} {
		if err := s.InsertOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
	}

	holding, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Do(ctx, func(ctx context.Context, r application.Repository) error {
			if _, err := r.LockOperation(ctx, locked.ID); err != nil {
				return err
			}
			close(holding)
			<-release

			return nil
		})
	}()

	<-holding
	got, err := s.ClaimDue(ctx, base.Add(time.Hour), time.Hour, 10) // must not block on the locked row
	close(release)
	<-done

	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != free.ID {
		t.Errorf("claimed %v, want only the unlocked operation", got)
	}
}
