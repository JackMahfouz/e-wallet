package application_test

import (
	"errors"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

func wantConfirmation(t testing.TB, err error, scope domain.LimitScope) *domain.ConfirmationRequiredError {
	t.Helper()

	var confirm *domain.ConfirmationRequiredError
	if !errors.As(err, &confirm) {
		t.Fatalf("err = %v, want a confirmation to be required", err)
	}
	if confirm.Scope != scope {
		t.Fatalf("confirmation scope = %s, want %s", confirm.Scope, scope)
	}

	return confirm
}

func TestSpendingWithinTheAccountLimitNeedsNoConfirmation(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 300)

	for _, amount := range []int64{100, 100, 100} { // exactly up to the limit
		op, err := w.withdraw(w.ada, a.ID, amount)
		must(t, err)
		wantStatus(t, op, domain.StatusCompleted)
	}

	if got := w.view(t, w.ada, a.ID).SpentToday; got != 300 {
		t.Errorf("spent today = %d, want 300", got)
	}
}

func TestSpendingOverTheAccountLimitIsSavedAndWaitsForConfirmation(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 300)

	_, err := w.withdraw(w.ada, a.ID, 200)
	must(t, err)

	op, err := w.withdraw(w.ada, a.ID, 150) // 350 > 300
	confirm := wantConfirmation(t, err, domain.ScopeAccount)

	if confirm.OperationID != op.ID || !confirm.ExpiresAt.Equal(w.Clock.Now().Add(10*time.Minute)) {
		t.Errorf("confirmation = %+v, operation %s", confirm, op.ID)
	}
	wantStatus(t, op, domain.StatusAwaitingConfirmation)
	if op.LimitScope != domain.ScopeAccount {
		t.Errorf("limit scope = %s", op.LimitScope)
	}

	// Nothing moved, nothing was reserved, and nothing counts as spent.
	wantBalance(t, w, a.ID, 800)
	view := w.view(t, w.ada, a.ID)
	if view.SpentToday != 200 || view.Held != 0 || view.Available != 800 {
		t.Errorf("view = %+v", view)
	}
	if w.Ledger.CallCount("Debit") != 1 {
		t.Errorf("debits = %d: the over-limit request must not reach the ledger", w.Ledger.CallCount("Debit"))
	}
}

func TestSpendingOverTheWalletLimitSpansAccounts(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.ada, "b", 1000)
	_, err := w.Svc.SetWalletLimit(w.ctx, w.ada, testsupport.ValidOTP, application.LimitChange{Set: true, Value: i64(500)})
	must(t, err)

	_, err = w.withdraw(w.ada, a.ID, 300)
	must(t, err)
	_, err = w.withdraw(w.ada, b.ID, 200) // 500 in all: exactly the limit
	must(t, err)

	op, err := w.withdraw(w.ada, b.ID, 1)
	wantConfirmation(t, err, domain.ScopeWallet)
	wantStatus(t, op, domain.StatusAwaitingConfirmation)

	wallet, err := w.Svc.Wallet(w.ctx, w.ada)
	must(t, err)
	if wallet.SpentToday != 500 {
		t.Errorf("wallet spent today = %d, want 500", wallet.SpentToday)
	}
}

func TestTheAccountLimitIsReportedBeforeTheWalletLimit(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	_, err := w.Svc.SetWalletLimit(w.ctx, w.ada, testsupport.ValidOTP, application.LimitChange{Set: true, Value: i64(100)})
	must(t, err)

	_, err = w.withdraw(w.ada, a.ID, 101)
	wantConfirmation(t, err, domain.ScopeAccount)
}

func TestMovingMoneyBetweenOwnAccountsNeverNeedsConfirmation(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 50)
	b := w.account(t, w.ada, "b", 0)

	op, err := w.transfer(w.ada, a.ID, b.ID, 900) // far over a's limit, but it stays in the wallet
	must(t, err)
	wantStatus(t, op, domain.StatusCompleted)
	if got := w.view(t, w.ada, a.ID).SpentToday; got != 0 {
		t.Errorf("spent today = %d", got)
	}

	// Sending it on to someone else is spending.
	c := w.account(t, w.bob, "c", 0)
	_, err = w.transfer(w.ada, b.ID, c.ID, 100)
	must(t, err) // b has no limit of its own, and there is no wallet limit
}

func TestCreditsAndDepositsAreNotSpending(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 0, 10)

	if _, err := w.deposit(w.ada, a.ID, 5000); err != nil {
		t.Errorf("deposit: %v", err)
	}
	if _, err := w.Svc.Credit(w.ctx, w.service("payment", a.ID, "", 5000)); err != nil {
		t.Errorf("credit: %v", err)
	}
	if got := w.view(t, w.ada, a.ID).SpentToday; got != 0 {
		t.Errorf("spent today = %d", got)
	}
}

func TestConfirmingNeedsAcknowledgementAndACodeThenCompletes(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	op, err := w.withdraw(w.ada, a.ID, 400)
	wantConfirmation(t, err, domain.ScopeAccount)

	// Without the explicit acknowledgement, or with the wrong code, nothing happens.
	_, err = w.Svc.Confirm(w.ctx, application.ConfirmRequest{UserID: w.ada, OperationID: op.ID, OTP: testsupport.ValidOTP})
	is(t, err, domain.ErrInvalid)
	_, err = w.Svc.Confirm(w.ctx, application.ConfirmRequest{UserID: w.ada, OperationID: op.ID, OTP: "000000", Acknowledged: true})
	is(t, err, domain.ErrOTPInvalid)
	wantStatus(t, w.operation(t, op.ID), domain.StatusAwaitingConfirmation)
	wantBalance(t, w, a.ID, 1000)

	done, err := w.confirm(w.ada, op.ID)
	must(t, err)
	wantStatus(t, done, domain.StatusCompleted)
	if !done.OverLimitConfirmed {
		t.Error("the operation should record that the user accepted going over the limit")
	}
	wantBalance(t, w, a.ID, 600)
	if got := w.view(t, w.ada, a.ID).SpentToday; got != 400 {
		t.Errorf("spent today = %d, want 400: a confirmed operation counts", got)
	}

	// Confirming again is harmless and changes nothing.
	again, err := w.confirm(w.ada, op.ID)
	must(t, err)
	wantStatus(t, again, domain.StatusCompleted)
	wantBalance(t, w, a.ID, 600)
	if w.Ledger.CallCount("Debit") != 1 {
		t.Errorf("debits = %d, want 1", w.Ledger.CallCount("Debit"))
	}
}

func TestAConfirmationExpiresAfterItsWindow(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	op, err := w.withdraw(w.ada, a.ID, 400)
	wantConfirmation(t, err, domain.ScopeAccount)

	w.Clock.Advance(10*time.Minute - time.Second)
	if _, err := w.Svc.Cancel(w.ctx, w.ada, op.ID); err != nil { // still open: cancelling works
		t.Fatalf("cancel inside the window: %v", err)
	}

	op, err = w.withdraw(w.ada, a.ID, 400)
	wantConfirmation(t, err, domain.ScopeAccount)
	w.Clock.Advance(10 * time.Minute)

	expired, err := w.confirm(w.ada, op.ID)
	is(t, err, domain.ErrConflict)
	wantFailed(t, expired, domain.FailureConfirmationExpired)
	wantFailed(t, w.operation(t, op.ID), domain.FailureConfirmationExpired) // and it stuck
	wantBalance(t, w, a.ID, 1000)
}

func TestRecoverySweepsConfirmationsThatNobodyAnswered(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	op, err := w.withdraw(w.ada, a.ID, 400)
	wantConfirmation(t, err, domain.ScopeAccount)

	if report := w.recoverAfter(t, 9*time.Minute); report.Expired != 0 {
		t.Errorf("expired = %d before the window closed", report.Expired)
	}
	wantStatus(t, w.operation(t, op.ID), domain.StatusAwaitingConfirmation)

	if report := w.recoverAfter(t, 2*time.Minute); report.Expired != 1 {
		t.Errorf("expired = %d, want 1", report.Expired)
	}
	wantFailed(t, w.operation(t, op.ID), domain.FailureConfirmationExpired)

	if report := w.recoverAfter(t, time.Minute); report.Expired != 0 {
		t.Errorf("a swept operation is swept once, got %d", report.Expired)
	}
}

func TestCancellingAnAwaitingOperation(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	op, err := w.withdraw(w.ada, a.ID, 400)
	wantConfirmation(t, err, domain.ScopeAccount)

	cancelled, err := w.Svc.Cancel(w.ctx, w.ada, op.ID) // no code needed
	must(t, err)
	wantFailed(t, cancelled, domain.FailureCancelled)

	if again, err := w.Svc.Cancel(w.ctx, w.ada, op.ID); err != nil || again.Failure != domain.FailureCancelled {
		t.Errorf("cancelling twice: %v, %+v", err, again)
	}
	_, err = w.confirm(w.ada, op.ID)
	if err == nil {
		t.Error("a cancelled operation cannot be confirmed")
	}
	wantFailed(t, w.operation(t, op.ID), domain.FailureCancelled)
	wantBalance(t, w, a.ID, 1000)

	// Only an awaiting operation can be cancelled.
	done, err := w.withdraw(w.ada, a.ID, 10)
	must(t, err)
	if _, err := w.Svc.Cancel(w.ctx, w.ada, done.ID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("cancelling a completed operation: %v", err)
	}
}

func TestConfirmingChecksTheFundsAgain(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	op, err := w.withdraw(w.ada, a.ID, 400)
	wantConfirmation(t, err, domain.ScopeAccount)

	// While it waited, the money went elsewhere: nothing had been reserved.
	w.Ledger.Fund(a.ID, -800)

	_, err = w.confirm(w.ada, op.ID)
	var insufficient *domain.InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want insufficient funds", err)
	}
	wantStatus(t, w.operation(t, op.ID), domain.StatusAwaitingConfirmation) // still there, to retry or cancel
}

func TestAReplayedOverLimitRequestAsksForTheSameConfirmation(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	req := application.UserMoneyRequest{UserID: w.ada, AccountID: a.ID, Amount: 400, IdempotencyKey: key(), OTP: testsupport.ValidOTP}

	first, err := w.Svc.Withdraw(w.ctx, req)
	wantConfirmation(t, err, domain.ScopeAccount)

	second, err := w.Svc.Withdraw(w.ctx, req)
	confirm := wantConfirmation(t, err, domain.ScopeAccount)
	if second.ID != first.ID || confirm.OperationID != first.ID {
		t.Errorf("the replay made a new operation: %s vs %s", second.ID, first.ID)
	}

	// Once the window has closed, the replay reports the failure instead.
	w.Clock.Advance(11 * time.Minute)
	third, err := w.Svc.Withdraw(w.ctx, req)
	must(t, err)
	wantFailed(t, third, domain.FailureConfirmationExpired)
}

func TestLimitsCountFromUTCMidnight(t *testing.T) {
	w := newWorld(t) // Wednesday 12:00 UTC
	a := w.limited(t, w.ada, "a", 1000, 100)

	_, err := w.withdraw(w.ada, a.ID, 100)
	must(t, err)
	_, err = w.withdraw(w.ada, a.ID, 1)
	wantConfirmation(t, err, domain.ScopeAccount)

	w.Clock.Advance(11*time.Hour + 59*time.Minute) // 23:59: still the same day
	_, err = w.withdraw(w.ada, a.ID, 1)
	wantConfirmation(t, err, domain.ScopeAccount)

	w.Clock.Advance(time.Minute) // midnight: a fresh day
	op, err := w.withdraw(w.ada, a.ID, 100)
	must(t, err)
	wantStatus(t, op, domain.StatusCompleted)
	if got := w.view(t, w.ada, a.ID).SpentToday; got != 100 {
		t.Errorf("spent today = %d, want 100", got)
	}
}

func TestOperationsThatFailDoNotCountAsSpent(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)

	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultRejected)
	op, err := w.withdraw(w.ada, a.ID, 100)
	must(t, err)
	wantFailed(t, op, domain.FailureRejected)

	w.Ledger.Fault = nil
	if got := w.view(t, w.ada, a.ID).SpentToday; got != 0 {
		t.Errorf("a failed operation counted %d as spent", got)
	}
	if done, err := w.withdraw(w.ada, a.ID, 100); err != nil || done.Status != domain.StatusCompleted {
		t.Errorf("the limit should still be free: %v, %+v", err, done)
	}
}

func TestServiceDebitOverTheLimitAlsoNeedsTheUsersConfirmation(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)

	op, err := w.Svc.Debit(w.ctx, w.service("payment", a.ID, "", 400))
	wantConfirmation(t, err, domain.ScopeAccount)
	wantStatus(t, op, domain.StatusAwaitingConfirmation)
	if op.Initiator != "service:payment" {
		t.Errorf("initiator = %q", op.Initiator)
	}

	// The service cannot confirm it; the account's owner does, over the user API.
	done, err := w.confirm(w.ada, op.ID)
	must(t, err)
	wantStatus(t, done, domain.StatusCompleted)
	wantBalance(t, w, a.ID, 600)
}

func TestAnOperationIsNeverExpiredBeforeItsWindowCloses(t *testing.T) {
	w := newWorld(t)
	a := w.limited(t, w.ada, "a", 1000, 100)
	op, err := w.withdraw(w.ada, a.ID, 400)
	wantConfirmation(t, err, domain.ScopeAccount)

	// Handed an operation that is not yet due (as it would be by a recovery pass that raced the
	// user confirming, or a clock that stepped back), expiring must do nothing.
	w.Clock.Advance(9 * time.Minute)
	if w.Svc.ExpireConfirmationForTest(w.ctx, op) {
		t.Fatal("an operation was expired before its confirmation window closed")
	}
	wantStatus(t, w.operation(t, op.ID), domain.StatusAwaitingConfirmation)

	w.Clock.Advance(time.Minute)
	if !w.Svc.ExpireConfirmationForTest(w.ctx, op) {
		t.Fatal("an operation past its window was not expired")
	}
	wantFailed(t, w.operation(t, op.ID), domain.FailureConfirmationExpired)

	if w.Svc.ExpireConfirmationForTest(w.ctx, op) {
		t.Error("expiring an operation twice should do nothing the second time")
	}
}
