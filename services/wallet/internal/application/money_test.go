package application_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

func TestDepositCollectsThenCredits(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)

	op, err := w.deposit(w.ada, a.ID, 2500)
	must(t, err)

	wantStatus(t, op, domain.StatusCompleted)
	if op.Kind != domain.KindDeposit || op.Amount != 2500 || op.AccountID != a.ID || op.Spending || op.CompletedAt == nil {
		t.Errorf("operation = %+v", op)
	}
	if w.Funding.Collected[op.ID] != 2500 {
		t.Errorf("collected = %v", w.Funding.Collected)
	}
	wantBalance(t, w, a.ID, 2500)
	if _, ok := w.Ledger.Posted(a.ID, op.ID, "credit"); !ok {
		t.Error("the credit must carry the operation id as its transaction id")
	}
	if op.Initiator != "user:"+w.ada {
		t.Errorf("initiator = %q", op.Initiator)
	}
}

func TestWithdrawalDebitsThenPaysOut(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)

	op, err := w.withdraw(w.ada, a.ID, 400)
	must(t, err)

	wantStatus(t, op, domain.StatusCompleted)
	if !op.Spending || op.Kind != domain.KindWithdrawal {
		t.Errorf("operation = %+v", op)
	}
	wantBalance(t, w, a.ID, 600)
	if w.Funding.PaidOut[op.ID] != 400 {
		t.Errorf("paid out = %v", w.Funding.PaidOut)
	}
}

func TestFundingCanBeSwitchedOff(t *testing.T) {
	w := newWorld(t, func(o *application.Options) { o.FundingEnabled = false })
	a := w.account(t, w.ada, "a", 1000)

	_, err := w.deposit(w.ada, a.ID, 100)
	is(t, err, domain.ErrFundingDisabled)
	_, err = w.withdraw(w.ada, a.ID, 100)
	is(t, err, domain.ErrFundingDisabled)

	// A wrong code is reported first, and nothing was recorded or moved.
	_, err = w.Svc.Deposit(w.ctx, application.UserMoneyRequest{UserID: w.ada, AccountID: a.ID, Amount: 1, IdempotencyKey: key(), OTP: "nope"})
	is(t, err, domain.ErrOTPInvalid)
	wantBalance(t, w, a.ID, 1000)
	if len(w.Funding.Collected)+len(w.Funding.PaidOut) != 0 {
		t.Error("the gateway must not be called while funding is off")
	}

	// Transfers and the internal API do not use the gateway.
	b := w.account(t, w.ada, "b", 0)
	if op, err := w.transfer(w.ada, a.ID, b.ID, 10); err != nil || op.Status != domain.StatusCompleted {
		t.Errorf("transfer with funding off: %v, %+v", err, op)
	}
	if _, err := w.Svc.Credit(w.ctx, w.service("payment", a.ID, "", 10)); err != nil {
		t.Errorf("service credit with funding off: %v", err)
	}
}

func TestTransferBetweenOwnAccountsMovesMoneyAndIsNotSpending(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.ada, "b", 0)

	op, err := w.transfer(w.ada, a.ID, b.ID, 300)
	must(t, err)

	wantStatus(t, op, domain.StatusCompleted)
	if op.Spending || op.Kind != domain.KindTransfer || op.CounterpartyID != b.ID {
		t.Errorf("operation = %+v", op)
	}
	wantBalance(t, w, a.ID, 700)
	wantBalance(t, w, b.ID, 300)

	// Both legs used the operation id as the ledger transaction id.
	if _, ok := w.Ledger.Posted(a.ID, op.ID, "debit"); !ok {
		t.Error("missing debit leg")
	}
	if _, ok := w.Ledger.Posted(b.ID, op.ID, "credit"); !ok {
		t.Error("missing credit leg")
	}
	if got := w.view(t, w.ada, a.ID).SpentToday; got != 0 {
		t.Errorf("spent today = %d, an internal transfer is not spending", got)
	}
}

func TestTransferToAnotherUserIsSpendingAndBothSidesSeeIt(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	op, err := w.transfer(w.ada, a.ID, b.ID, 250)
	must(t, err)

	wantStatus(t, op, domain.StatusCompleted)
	if !op.Spending {
		t.Error("money leaving the wallet is spending")
	}
	wantBalance(t, w, a.ID, 750)
	wantBalance(t, w, b.ID, 250)
	if got := w.view(t, w.ada, a.ID).SpentToday; got != 250 {
		t.Errorf("spent today = %d, want 250", got)
	}

	for _, user := range []string{w.ada, w.bob} {
		if got, err := w.Svc.Operation(w.ctx, user, op.ID); err != nil || got.ID != op.ID {
			t.Errorf("Operation as %s: %v", user, err)
		}
	}
	if _, err := w.Svc.Operation(w.ctx, w.stranger, op.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a stranger sees the operation: %v", err)
	}

	// The recipient is a party, not the owner: they can neither confirm nor cancel it.
	if _, err := w.Svc.Cancel(w.ctx, w.bob, op.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("recipient cancel: %v", err)
	}
	if _, err := w.confirm(w.bob, op.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("recipient confirm: %v", err)
	}
}

func TestInputValidation(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.ada, "b", 0)
	otp := testsupport.ValidOTP

	tests := map[string]func() error{
		"zero amount":      func() error { _, err := w.transfer(w.ada, a.ID, b.ID, 0); return err },
		"negative amount":  func() error { _, err := w.withdraw(w.ada, a.ID, -5); return err },
		"deposit of zero":  func() error { _, err := w.deposit(w.ada, a.ID, 0); return err },
		"transfer to self": func() error { _, err := w.transfer(w.ada, a.ID, a.ID, 5); return err },
		"bad source id":    func() error { _, err := w.transfer(w.ada, "42", b.ID, 5); return err },
		"bad dest id":      func() error { _, err := w.transfer(w.ada, a.ID, "nope", 5); return err },
		"missing key": func() error {
			_, err := w.Svc.Withdraw(w.ctx, application.UserMoneyRequest{UserID: w.ada, AccountID: a.ID, Amount: 5, OTP: otp})
			return err
		},
		"key not a uuid": func() error {
			_, err := w.Svc.Withdraw(w.ctx, application.UserMoneyRequest{UserID: w.ada, AccountID: a.ID, Amount: 5, IdempotencyKey: "abc", OTP: otp})
			return err
		},
		"service name":      func() error { _, err := w.Svc.Credit(w.ctx, w.service("Payment!", a.ID, "", 5)); return err },
		"service no name":   func() error { _, err := w.Svc.Credit(w.ctx, w.service("", a.ID, "", 5)); return err },
		"credit with peer":  func() error { _, err := w.Svc.Credit(w.ctx, w.service("payment", a.ID, b.ID, 5)); return err },
		"service self move": func() error { _, err := w.Svc.TransferBetween(w.ctx, w.service("payment", a.ID, a.ID, 5)); return err },
		"service no peer":   func() error { _, err := w.Svc.TransferBetween(w.ctx, w.service("payment", a.ID, "", 5)); return err },
		"credit with hold": func() error {
			req := w.service("payment", a.ID, "", 5)
			req.HoldID = testsupport.UUID(77)

			_, err := w.Svc.Credit(w.ctx, req)

			return err
		},
	}

	for name, call := range tests {
		if err := call(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	wantBalance(t, w, a.ID, 1000)
}

func TestInsufficientFundsIsRefusedBeforeAnythingIsRecorded(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 100)
	b := w.account(t, w.bob, "b", 0)

	for name, call := range map[string]func() error{
		"transfer":   func() error { _, err := w.transfer(w.ada, a.ID, b.ID, 101); return err },
		"withdrawal": func() error { _, err := w.withdraw(w.ada, a.ID, 101); return err },
		"debit":      func() error { _, err := w.Svc.Debit(w.ctx, w.service("payment", a.ID, "", 101)); return err },
	} {
		var insufficient *domain.InsufficientFundsError
		err := call()
		if !errors.As(err, &insufficient) || insufficient.Available != 100 || insufficient.Requested != 101 {
			t.Errorf("%s: err = %v, want insufficient funds (available 100, requested 101)", name, err)
		}
	}

	if ops, _ := w.Svc.Transactions(w.ctx, w.ada, a.ID, nil, 10); len(ops) != 0 {
		t.Errorf("a refused request left %d operation(s)", len(ops))
	}
	if w.Ledger.CallCount("Debit") != 0 {
		t.Error("the ledger must not be asked to debit money the wallet already knows is not there")
	}

	// The whole balance can be spent.
	op, err := w.withdraw(w.ada, a.ID, 100)
	must(t, err)
	wantStatus(t, op, domain.StatusCompleted)
}

func TestRetryWithTheSameKeyReturnsTheSameOperationAndMovesMoneyOnce(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	req := application.TransferRequest{UserID: w.ada, FromAccountID: a.ID, ToAccountID: b.ID, Amount: 100, IdempotencyKey: key(), OTP: testsupport.ValidOTP}

	first, err := w.Svc.Transfer(w.ctx, req)
	must(t, err)
	again, err := w.Svc.Transfer(w.ctx, req)
	must(t, err)

	if first.ID != again.ID || again.Status != domain.StatusCompleted {
		t.Errorf("retry returned %s (%s), want %s (completed)", again.ID, again.Status, first.ID)
	}
	wantBalance(t, w, a.ID, 900)
	wantBalance(t, w, b.ID, 100)
	if n := w.Ledger.CallCount("Debit"); n != 1 {
		t.Errorf("ledger debits = %d, want 1", n)
	}

	// The same key with a different request is a mistake, not a retry.
	for name, change := range map[string]func(*application.TransferRequest){
		"amount":      func(r *application.TransferRequest) { r.Amount = 101 },
		"destination": func(r *application.TransferRequest) { r.ToAccountID = w.account(t, w.bob, "other", 0).ID },
	} {
		changed := req
		change(&changed)
		if _, err := w.Svc.Transfer(w.ctx, changed); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
			t.Errorf("changed %s: err = %v, want ErrIdempotencyKeyReused", name, err)
		}
	}
	wantBalance(t, w, a.ID, 900)

	// Keys are per initiator: another user may use the same key, and so may a service.
	c := w.account(t, w.bob, "c", 500)
	if _, err := w.Svc.Transfer(w.ctx, application.TransferRequest{UserID: w.bob, FromAccountID: c.ID, ToAccountID: a.ID, Amount: 5, IdempotencyKey: req.IdempotencyKey, OTP: testsupport.ValidOTP}); err != nil {
		t.Errorf("same key, other user: %v", err)
	}
	svc := w.service("payment", c.ID, "", 5)
	svc.IdempotencyKey = req.IdempotencyKey
	if _, err := w.Svc.Debit(w.ctx, svc); err != nil {
		t.Errorf("same key, a service: %v", err)
	}
}

func TestARetryDrivesAStalledOperationOn(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultUnavailable)
	req := application.TransferRequest{UserID: w.ada, FromAccountID: a.ID, ToAccountID: b.ID, Amount: 100, IdempotencyKey: key(), OTP: testsupport.ValidOTP}

	stalled, err := w.Svc.Transfer(w.ctx, req)
	must(t, err)
	wantStatus(t, stalled, domain.StatusPending)

	w.Ledger.Fault = nil
	retried, err := w.Svc.Transfer(w.ctx, req)
	must(t, err)

	if retried.ID != stalled.ID {
		t.Errorf("the retry made a new operation")
	}
	wantStatus(t, retried, domain.StatusCompleted)
	wantBalance(t, w, a.ID, 900)
	wantBalance(t, w, b.ID, 100)
}

func TestServiceCreditAndDebit(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)

	credit, err := w.Svc.Credit(w.ctx, w.service("payment", a.ID, "", 700))
	must(t, err)
	wantStatus(t, credit, domain.StatusCompleted)
	if credit.Initiator != "service:payment" || credit.Spending {
		t.Errorf("credit = %+v", credit)
	}

	debit, err := w.Svc.Debit(w.ctx, w.service("payment", a.ID, "", 200))
	must(t, err)
	wantStatus(t, debit, domain.StatusCompleted)
	if !debit.Spending {
		t.Error("a debit is spending")
	}
	wantBalance(t, w, a.ID, 500)

	b := w.account(t, w.bob, "b", 0)
	moved, err := w.Svc.TransferBetween(w.ctx, w.service("payment", a.ID, b.ID, 100))
	must(t, err)
	wantStatus(t, moved, domain.StatusCompleted)
	wantBalance(t, w, b.ID, 100)

	// Any service can read an operation and an account.
	if got, err := w.Svc.OperationFor(w.ctx, debit.ID); err != nil || got.ID != debit.ID {
		t.Errorf("OperationFor: %v", err)
	}
	if view, err := w.Svc.AccountFor(w.ctx, a.ID); err != nil || view.Balance != 400 {
		t.Errorf("AccountFor: %+v, %v", view, err)
	}
	if _, err := w.Svc.AccountFor(w.ctx, testsupport.UUID(99)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("AccountFor unknown: %v", err)
	}
	if _, err := w.Svc.OperationFor(w.ctx, testsupport.UUID(99)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("OperationFor unknown: %v", err)
	}
}

func TestOperationDeadlineIsFiveWorkingDaysOut(t *testing.T) {
	w := newWorld(t) // the fake clock starts on Wednesday 11 November 2026, 12:00 UTC
	a := w.account(t, w.ada, "a", 0)

	op, err := w.deposit(w.ada, a.ID, 10)
	must(t, err)

	if want := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC); !op.DeadlineAt.Equal(want) {
		t.Errorf("deadline = %s, want %s", op.DeadlineAt, want)
	}

	w.Clock.Set(time.Date(2026, time.November, 13, 12, 0, 0, 0, time.UTC)) // a Friday
	op, err = w.deposit(w.ada, a.ID, 10)
	must(t, err)
	if want := time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC); !op.DeadlineAt.Equal(want) {
		t.Errorf("deadline from a Friday = %s, want %s", op.DeadlineAt, want)
	}
}

func TestConcurrentWithdrawalsNeverOverdrawOrOverspend(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 100)

	const attempts = 30
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

			op, err := w.withdraw(w.ada, a.ID, 10)

			mu.Lock()
			defer mu.Unlock()
			var insufficient *domain.InsufficientFundsError
			switch {
			case err == nil && op.Status == domain.StatusCompleted:
				completed++
			case errors.As(err, &insufficient):
				refused++
			default:
				t.Errorf("unexpected result: %v, %+v", err, op)
			}
		}()
	}
	wg.Wait()

	if completed != 10 || refused != attempts-10 {
		t.Errorf("completed = %d, refused = %d; want 10 and %d", completed, refused, attempts-10)
	}
	wantBalance(t, w, a.ID, 0)
}

func TestConcurrentTransfersConserveMoney(t *testing.T) {
	w := newWorld(t)
	accounts := []string{
		w.account(t, w.ada, "a", 500).ID, w.account(t, w.ada, "b", 500).ID,
		w.account(t, w.bob, "c", 500).ID, w.account(t, w.bob, "d", 500).ID,
	}
	owner := map[string]string{accounts[0]: w.ada, accounts[1]: w.ada, accounts[2]: w.bob, accounts[3]: w.bob}

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		from, to := accounts[i%4], accounts[(i*3+1)%4]
		if from == to {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.transfer(owner[from], from, to, 70)
		}()
	}
	wg.Wait()

	if total := w.Ledger.Total(); total != 2000 {
		t.Errorf("total = %d, want 2000: money was created or destroyed", total)
	}
	for _, id := range accounts {
		if got := w.Ledger.BalanceOf(id); got < 0 {
			t.Errorf("account %s overdrawn: %d", id, got)
		}
	}
}
