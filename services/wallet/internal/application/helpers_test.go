package application_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

// world is a wired-up Service with two users, ada and bob, and a stranger who has no wallet.
type world struct {
	*testsupport.Env
	ctx      context.Context
	ada, bob string
	stranger string
}

func newWorld(t *testing.T, mutate ...func(*application.Options)) *world {
	t.Helper()

	return &world{
		Env: testsupport.NewEnv(t, mutate...), ctx: context.Background(),
		ada: testsupport.UUID(1), bob: testsupport.UUID(2), stranger: testsupport.UUID(3),
	}
}

var keyCounter atomic.Int64

// key returns a fresh idempotency key.
func key() string { return testsupport.UUID(int(100_000 + keyCounter.Add(1))) }

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

// account makes an account for user, with balance cents put straight into the ledger.
func (w *world) account(t testing.TB, user, name string, balance int64) application.AccountView {
	t.Helper()

	view, err := w.Svc.CreateAccount(w.ctx, user, name, nil)
	must(t, err)
	if balance > 0 {
		w.Ledger.Fund(view.ID, balance)
	}

	return view
}

func (w *world) limited(t testing.TB, user, name string, balance, limit int64) application.AccountView {
	t.Helper()

	view, err := w.Svc.CreateAccount(w.ctx, user, name, &limit)
	must(t, err)
	if balance > 0 {
		w.Ledger.Fund(view.ID, balance)
	}

	return view
}

func (w *world) transfer(user, from, to string, amount int64) (domain.Operation, error) {
	return w.Svc.Transfer(w.ctx, application.TransferRequest{
		UserID: user, FromAccountID: from, ToAccountID: to, Amount: amount,
		IdempotencyKey: key(), OTP: testsupport.ValidOTP,
	})
}

func (w *world) withdraw(user, account string, amount int64) (domain.Operation, error) {
	return w.Svc.Withdraw(w.ctx, application.UserMoneyRequest{
		UserID: user, AccountID: account, Amount: amount, IdempotencyKey: key(), OTP: testsupport.ValidOTP,
	})
}

func (w *world) deposit(user, account string, amount int64) (domain.Operation, error) {
	return w.Svc.Deposit(w.ctx, application.UserMoneyRequest{
		UserID: user, AccountID: account, Amount: amount, IdempotencyKey: key(), OTP: testsupport.ValidOTP,
	})
}

func (w *world) confirm(user, operation string) (domain.Operation, error) {
	return w.Svc.Confirm(w.ctx, application.ConfirmRequest{
		UserID: user, OperationID: operation, OTP: testsupport.ValidOTP, Acknowledged: true,
	})
}

func (w *world) view(t testing.TB, user, account string) application.AccountView {
	t.Helper()

	view, err := w.Svc.Account(w.ctx, user, account)
	must(t, err)

	return view
}

func (w *world) operation(t testing.TB, id string) domain.Operation {
	t.Helper()

	op, err := w.Store.OperationByID(w.ctx, id)
	must(t, err)

	return op
}

func (w *world) service(service, account, counterparty string, amount int64) application.ServiceMoneyRequest {
	return application.ServiceMoneyRequest{
		Service: service, AccountID: account, CounterpartyID: counterparty, Amount: amount, IdempotencyKey: key(),
	}
}

// recoverAfter moves the clock on and runs one recovery pass.
func (w *world) recoverAfter(t testing.TB, d time.Duration) application.RecoveryReport {
	t.Helper()

	w.Clock.Advance(d)
	report, err := w.Svc.Recover(w.ctx)
	must(t, err)

	return report
}

func wantStatus(t testing.TB, op domain.Operation, status domain.OperationStatus) {
	t.Helper()

	if op.Status != status {
		t.Fatalf("operation is %s (failure %s), want %s", op.Status, op.Failure, status)
	}
}

func wantFailed(t testing.TB, op domain.Operation, reason domain.FailureReason) {
	t.Helper()

	if op.Status != domain.StatusFailed || op.Failure != reason {
		t.Fatalf("operation is %s (failure %s), want failed(%s)", op.Status, op.Failure, reason)
	}
}

func wantBalance(t testing.TB, w *world, account string, cents int64) {
	t.Helper()

	if got := w.Ledger.BalanceOf(account); got != cents {
		t.Fatalf("ledger balance = %d, want %d", got, cents)
	}
}

func i64(v int64) *int64 { return &v }

func onlyFault(method string, fault testsupport.Fault) func(testsupport.LedgerCall) testsupport.Fault {
	return func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == method {
			return fault
		}

		return testsupport.FaultNone
	}
}
