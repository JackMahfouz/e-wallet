package application_test

import (
	"errors"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

func (w *world) hold(t testing.TB, service, account string, amount int64, ttl time.Duration) domain.Hold {
	t.Helper()

	hold, err := w.Svc.CreateHold(w.ctx, application.CreateHoldRequest{
		Service: service, AccountID: account, Amount: amount, Reason: "order 17", TTL: ttl, IdempotencyKey: key(),
	})
	must(t, err)

	return hold
}

func TestAHoldReservesMoneyWithoutMovingIt(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)

	hold := w.hold(t, "payment", a.ID, 400, time.Hour)

	if hold.Amount != 400 || hold.Initiator != "service:payment" || hold.Reason != "order 17" || !hold.ExpiresAt.Equal(w.Clock.Now().Add(time.Hour)) {
		t.Errorf("hold = %+v", hold)
	}
	view := w.view(t, w.ada, a.ID)
	if view.Balance != 1000 || view.Held != 400 || view.Available != 600 {
		t.Errorf("view = balance %d, held %d, available %d", view.Balance, view.Held, view.Available)
	}
	wantBalance(t, w, a.ID, 1000)

	// The reserved money cannot be spent by anyone else, but the rest can.
	_, err := w.withdraw(w.ada, a.ID, 601)
	var insufficient *domain.InsufficientFundsError
	if !errors.As(err, &insufficient) || insufficient.Available != 600 {
		t.Errorf("err = %v, want insufficient funds with 600 available", err)
	}
	if _, err := w.withdraw(w.ada, a.ID, 600); err != nil {
		t.Errorf("withdrawing the unreserved part: %v", err)
	}

	// A hold needs money that is actually free.
	_, err = w.Svc.CreateHold(w.ctx, application.CreateHoldRequest{Service: "payment", AccountID: a.ID, Amount: 1, TTL: time.Hour, IdempotencyKey: key()})
	if !errors.As(err, &insufficient) {
		t.Errorf("a hold on money that is all reserved or spent: %v", err)
	}
}

func TestHoldRequestsAreValidated(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	good := application.CreateHoldRequest{Service: "payment", AccountID: a.ID, Amount: 10, TTL: time.Hour}

	tests := map[string]func(*application.CreateHoldRequest){
		"no key":      func(r *application.CreateHoldRequest) { r.IdempotencyKey = "" },
		"zero amount": func(r *application.CreateHoldRequest) { r.Amount = 0 },
		"no ttl":      func(r *application.CreateHoldRequest) { r.TTL = 0 },
		"ttl too long": func(r *application.CreateHoldRequest) {
			r.TTL = w.Options.MaxHoldTTL + time.Second
		},
		"bad service": func(r *application.CreateHoldRequest) { r.Service = "Pay Ment" },
		"bad account": func(r *application.CreateHoldRequest) { r.AccountID = "x" },
		"long reason": func(r *application.CreateHoldRequest) { r.Reason = string(make([]byte, 201)) },
	}

	for name, mutate := range tests {
		req := good
		req.IdempotencyKey = key()
		mutate(&req)

		if _, err := w.Svc.CreateHold(w.ctx, req); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	req := good
	req.IdempotencyKey = key()
	req.AccountID = testsupport.UUID(99)
	if _, err := w.Svc.CreateHold(w.ctx, req); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown account: %v", err)
	}

	// The longest allowed TTL is allowed.
	req = good
	req.IdempotencyKey = key()
	req.TTL = w.Options.MaxHoldTTL
	if _, err := w.Svc.CreateHold(w.ctx, req); err != nil {
		t.Errorf("max ttl: %v", err)
	}
}

func TestCreatingAHoldIsIdempotent(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	req := application.CreateHoldRequest{Service: "payment", AccountID: a.ID, Amount: 300, TTL: time.Hour, IdempotencyKey: key()}

	first, err := w.Svc.CreateHold(w.ctx, req)
	must(t, err)
	again, err := w.Svc.CreateHold(w.ctx, req)
	must(t, err)

	if first.ID != again.ID {
		t.Error("a retried request made a second hold")
	}
	if held := w.view(t, w.ada, a.ID).Held; held != 300 {
		t.Errorf("held = %d, want 300", held)
	}

	req.Amount = 301
	if _, err := w.Svc.CreateHold(w.ctx, req); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Errorf("same key, different amount: %v", err)
	}
}

func TestReleasingAndExpiringAHoldFreesTheMoney(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)

	released := w.hold(t, "payment", a.ID, 400, time.Hour)
	expiring := w.hold(t, "payment", a.ID, 300, time.Minute)
	if held := w.view(t, w.ada, a.ID).Held; held != 700 {
		t.Fatalf("held = %d, want 700", held)
	}

	w.Clock.Advance(time.Minute) // the second hold lapses by itself
	if held := w.view(t, w.ada, a.ID).Held; held != 400 {
		t.Errorf("held after expiry = %d, want 400", held)
	}

	out, err := w.Svc.ReleaseHold(w.ctx, "payment", released.ID)
	must(t, err)
	if out.ReleasedAt == nil {
		t.Error("the released hold should say when")
	}
	if view := w.view(t, w.ada, a.ID); view.Held != 0 || view.Available != 1000 {
		t.Errorf("view = %+v", view)
	}

	// Releasing again is fine, and a lapsed hold can still be released.
	if _, err := w.Svc.ReleaseHold(w.ctx, "payment", released.ID); err != nil {
		t.Errorf("releasing twice: %v", err)
	}
	if _, err := w.Svc.ReleaseHold(w.ctx, "payment", expiring.ID); err != nil {
		t.Errorf("releasing an expired hold: %v", err)
	}

	// Another service cannot touch it, and it is not told the hold exists.
	other := w.hold(t, "payment", a.ID, 10, time.Hour)
	if _, err := w.Svc.ReleaseHold(w.ctx, "refunds", other.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another service releasing: %v", err)
	}
	if _, err := w.Svc.ReleaseHold(w.ctx, "payment", testsupport.UUID(99)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown hold: %v", err)
	}
}

func TestADebitCapturesItsHold(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	hold := w.hold(t, "payment", a.ID, 400, time.Hour)

	req := w.service("payment", a.ID, "", 350)
	req.HoldID = hold.ID
	op, err := w.Svc.Debit(w.ctx, req)
	must(t, err)

	wantStatus(t, op, domain.StatusCompleted)
	wantBalance(t, w, a.ID, 650)
	captured, err := w.Store.HoldByID(w.ctx, hold.ID)
	must(t, err)
	if captured.CapturedBy != op.ID {
		t.Errorf("hold captured by %q, want %q", captured.CapturedBy, op.ID)
	}
	// The whole hold is consumed, including the 50 not debited.
	if view := w.view(t, w.ada, a.ID); view.Held != 0 || view.Available != 650 {
		t.Errorf("view = %+v", view)
	}

	// A hold is used once.
	again := w.service("payment", a.ID, "", 10)
	again.HoldID = hold.ID
	if _, err := w.Svc.Debit(w.ctx, again); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("reusing a captured hold: %v", err)
	}
	if _, err := w.Svc.ReleaseHold(w.ctx, "payment", hold.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("releasing a captured hold: %v", err)
	}
}

func TestCapturingNeedsTheRightActiveHoldBigEnough(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.ada, "b", 1000)
	hold := w.hold(t, "payment", a.ID, 100, time.Minute)

	capture := func(account, holdID string, amount int64) error {
		req := w.service("payment", account, "", amount)
		req.HoldID = holdID
		_, err := w.Svc.Debit(w.ctx, req)

		return err
	}

	if err := capture(a.ID, hold.ID, 101); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("more than the hold: %v", err)
	}
	if err := capture(b.ID, hold.ID, 10); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("another account's hold: %v", err)
	}
	if err := capture(a.ID, testsupport.UUID(99), 10); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown hold: %v", err)
	}

	w.Clock.Advance(time.Minute)
	if err := capture(a.ID, hold.ID, 10); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("an expired hold: %v", err)
	}
	wantBalance(t, w, a.ID, 1000)
}

func TestATransferCanCaptureAHoldToo(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)
	hold := w.hold(t, "payment", a.ID, 500, time.Hour)

	req := w.service("payment", a.ID, b.ID, 500)
	req.HoldID = hold.ID
	op, err := w.Svc.TransferBetween(w.ctx, req)
	must(t, err)

	wantStatus(t, op, domain.StatusCompleted)
	wantBalance(t, w, a.ID, 500)
	wantBalance(t, w, b.ID, 500)
}

func TestAFailedOperationGivesItsHoldBack(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	hold := w.hold(t, "payment", a.ID, 400, time.Hour)

	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultRejected)
	req := w.service("payment", a.ID, "", 400)
	req.HoldID = hold.ID
	op, err := w.Svc.Debit(w.ctx, req)
	must(t, err)
	wantFailed(t, op, domain.FailureRejected)

	back, err := w.Store.HoldByID(w.ctx, hold.ID)
	must(t, err)
	if back.CapturedBy != "" {
		t.Errorf("a failed operation kept the hold captured by %q", back.CapturedBy)
	}
	if held := w.view(t, w.ada, a.ID).Held; held != 400 {
		t.Errorf("held = %d, want the hold active again", held)
	}

	// The hold can be tried again with a new request.
	w.Ledger.Fault = nil
	retry := w.service("payment", a.ID, "", 400)
	retry.HoldID = hold.ID
	done, err := w.Svc.Debit(w.ctx, retry)
	must(t, err)
	wantStatus(t, done, domain.StatusCompleted)
}

func TestAnOverLimitDebitCapturesItsHoldOnlyWhenConfirmed(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	hold := w.hold(t, "payment", a.ID, 400, time.Hour)

	req := w.service("payment", a.ID, "", 400)
	req.HoldID = hold.ID
	op, err := w.Svc.Debit(w.ctx, req)
	wantConfirmation(t, err, domain.ScopeAccount)

	waiting, err := w.Store.HoldByID(w.ctx, hold.ID)
	must(t, err)
	if waiting.CapturedBy != "" {
		t.Error("nothing is captured while the operation waits for confirmation")
	}

	done, err := w.confirm(w.ada, op.ID)
	must(t, err)
	wantStatus(t, done, domain.StatusCompleted)
	captured, err := w.Store.HoldByID(w.ctx, hold.ID)
	must(t, err)
	if captured.CapturedBy != op.ID {
		t.Errorf("hold captured by %q, want %q", captured.CapturedBy, op.ID)
	}
	wantBalance(t, w, a.ID, 600)
}

func TestAHoldCanReserveExactlyWhatIsAvailableButNotACentMore(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)

	w.hold(t, "payment", a.ID, 400, time.Hour)
	w.hold(t, "payment", a.ID, 600, time.Hour) // exactly the 600 still available

	if view := w.view(t, w.ada, a.ID); view.Held != 1000 || view.Available != 0 {
		t.Errorf("view = %+v, want everything held and nothing available", view)
	}

	_, err := w.Svc.CreateHold(w.ctx, application.CreateHoldRequest{Service: "payment", AccountID: a.ID, Amount: 1, TTL: time.Hour, IdempotencyKey: key()})
	var insufficient *domain.InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Errorf("a hold on money that is all reserved: %v, want insufficient funds", err)
	}
}
