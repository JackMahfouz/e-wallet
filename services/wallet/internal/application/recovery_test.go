package application_test

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

// eight days is more than five working days from any starting day.
const pastTheDeadline = 8 * 24 * time.Hour

// firstOnly injects fault into the first call of method only, then lets everything through.
func firstOnly(method string, fault testsupport.Fault) func(testsupport.LedgerCall) testsupport.Fault {
	used := false

	return func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == method && !used {
			used = true

			return fault
		}

		return testsupport.FaultNone
	}
}

func TestLedgerDownBeforeAnythingIsRecordedRefusesCleanly(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = func(testsupport.LedgerCall) testsupport.Fault { return testsupport.FaultUnavailable }
	req := w.service("payment", a.ID, b.ID, 100)

	_, err := w.Svc.TransferBetween(w.ctx, req)
	is(t, err, domain.ErrUnavailable)

	if _, err := w.Store.OperationByKey(w.ctx, "service:payment", req.IdempotencyKey); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a request refused with 503 left a record: %v", err)
	}
	if _, err := w.Svc.Account(w.ctx, w.ada, a.ID); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("reading an account while the ledger is down: %v", err)
	}
}

func TestADebitThatCannotReachTheLedgerIsRetriedByRecovery(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultUnavailable)
	op, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err) // accepted, not finished
	wantStatus(t, op, domain.StatusPending)
	wantBalance(t, w, a.ID, 1000)

	// New operations are left to their own request for a while.
	if report, err := w.Svc.Recover(w.ctx); err != nil || report.Advanced != 0 {
		t.Fatalf("recovery touched a fresh operation: %+v, %v", report, err)
	}

	// Later it is picked up, and with the ledger still down it is left alone again, for longer.
	if report := w.recoverAfter(t, time.Minute); report.Advanced != 1 {
		t.Fatalf("advanced = %d, want 1", report.Advanced)
	}
	wantStatus(t, w.operation(t, op.ID), domain.StatusPending)
	if report, err := w.Svc.Recover(w.ctx); err != nil || report.Advanced != 0 {
		t.Fatalf("a claimed operation was picked up again at once: %+v, %v", report, err)
	}

	w.Ledger.Fault = nil
	if report := w.recoverAfter(t, time.Hour); report.Advanced != 1 {
		t.Fatalf("advanced = %d, want 1", report.Advanced)
	}
	done := w.operation(t, op.ID)
	wantStatus(t, done, domain.StatusCompleted)
	if done.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", done.Attempts)
	}
	wantBalance(t, w, a.ID, 900)
	wantBalance(t, w, b.ID, 100)
}

func TestALostDebitResponseIsNotDebitedTwice(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = firstOnly("Debit", testsupport.FaultLostResponse) // applied, but we were not told
	op, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err)
	wantStatus(t, op, domain.StatusPending)
	wantBalance(t, w, a.ID, 900) // the debit did happen

	w.recoverAfter(t, time.Minute)

	wantStatus(t, w.operation(t, op.ID), domain.StatusCompleted)
	wantBalance(t, w, a.ID, 900) // and was not repeated
	wantBalance(t, w, b.ID, 100)
}

func TestACreditThatCannotReachTheLedgerLeavesTheMoneyInTransitUntilItCan(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = onlyFault("Credit", testsupport.FaultUnavailable)
	op, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err)
	wantStatus(t, op, domain.StatusSettling)
	wantBalance(t, w, a.ID, 900)
	wantBalance(t, w, b.ID, 0)

	// Days pass with the ledger still down: the money has left the source, so the operation is not
	// failed, because there is nothing safe to fail it with.
	w.recoverAfter(t, pastTheDeadline)
	wantStatus(t, w.operation(t, op.ID), domain.StatusSettling)

	w.Ledger.Fault = nil
	w.recoverAfter(t, time.Hour)
	wantStatus(t, w.operation(t, op.ID), domain.StatusCompleted)
	wantBalance(t, w, b.ID, 100)
}

func TestALostCreditResponseDoesNotCreditTwice(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = firstOnly("Credit", testsupport.FaultLostResponse)
	op, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err)
	wantStatus(t, op, domain.StatusSettling)
	wantBalance(t, w, b.ID, 100)

	w.recoverAfter(t, time.Minute)
	wantStatus(t, w.operation(t, op.ID), domain.StatusCompleted)
	wantBalance(t, w, b.ID, 100)
}

func TestAStatusThatCannotBeRecordedIsRecordedOnTheNextAttempt(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	failures := 0
	w.Store.FailSaveOperation = func(op domain.Operation) error {
		if op.Status == domain.StatusSettling && failures == 0 {
			failures++

			return errors.New("database went away")
		}

		return nil
	}

	op, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err)
	wantStatus(t, op, domain.StatusPending) // the ledger debited, the record of it was lost
	wantBalance(t, w, a.ID, 900)

	w.recoverAfter(t, time.Minute)
	wantStatus(t, w.operation(t, op.ID), domain.StatusCompleted)
	wantBalance(t, w, a.ID, 900)
	wantBalance(t, w, b.ID, 100)
}

func TestAnOperationOutOfTimeIsRefundedAndFailedOnceTheLedgerAnswers(t *testing.T) {
	for name, fault := range map[string]testsupport.Fault{
		"the debit never reached the ledger": testsupport.FaultUnavailable,
		"the debit reached it, unannounced":  testsupport.FaultLostResponse,
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			a := w.account(t, w.ada, "a", 1000)
			b := w.account(t, w.bob, "b", 0)

			w.Ledger.Fault = firstOnly("Debit", fault)
			op, err := w.transfer(w.ada, a.ID, b.ID, 100)
			must(t, err)
			wantStatus(t, op, domain.StatusPending)

			w.recoverAfter(t, pastTheDeadline)

			failed := w.operation(t, op.ID)
			wantFailed(t, failed, domain.FailureTimedOut)
			if failed.RefundedAt == nil {
				t.Error("the refund should be recorded")
			}
			wantBalance(t, w, a.ID, 1000) // nothing lost
			wantBalance(t, w, b.ID, 0)    // nothing arrived
			if _, ok := w.Ledger.Posted(a.ID, op.ID, "credit"); !ok {
				t.Error("the refund is a credit carrying the operation id")
			}
		})
	}
}

func TestAnOperationOutOfTimeIsNotFailedWhileTheLedgerStaysDown(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultUnavailable)
	op, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err)

	// Whether a debit reached the ledger cannot be known without asking it, so the operation is
	// kept rather than failed on a guess.
	w.recoverAfter(t, pastTheDeadline)
	wantStatus(t, w.operation(t, op.ID), domain.StatusPending)

	w.Ledger.Fault = nil
	w.recoverAfter(t, time.Hour)
	wantFailed(t, w.operation(t, op.ID), domain.FailureTimedOut)
	wantBalance(t, w, a.ID, 1000)
}

func TestWithdrawalsWhoseMoneyCannotBePaidOutAreRefunded(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)

	w.Funding.Fault = func(method, _ string) testsupport.Fault {
		if method == "payout" {
			return testsupport.FaultUnavailable
		}

		return testsupport.FaultNone
	}
	op, err := w.withdraw(w.ada, a.ID, 400)
	must(t, err)
	wantStatus(t, op, domain.StatusSettling)
	wantBalance(t, w, a.ID, 600)

	// Before the deadline it keeps trying; after it, the money goes back.
	w.recoverAfter(t, time.Hour)
	wantStatus(t, w.operation(t, op.ID), domain.StatusSettling)

	w.recoverAfter(t, pastTheDeadline)
	failed := w.operation(t, op.ID)
	wantFailed(t, failed, domain.FailureTimedOut)
	wantBalance(t, w, a.ID, 1000)
	if failed.RefundedAt == nil {
		t.Error("the refund should be recorded")
	}
}

func TestARejectedPayoutRefundsAtOnce(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)

	w.Funding.Fault = func(method, _ string) testsupport.Fault {
		if method == "payout" {
			return testsupport.FaultRejected
		}

		return testsupport.FaultNone
	}
	op, err := w.withdraw(w.ada, a.ID, 400)
	must(t, err)

	wantFailed(t, op, domain.FailureRejected)
	wantBalance(t, w, a.ID, 1000)
}

func TestADepositThatCannotBeCollectedFailsWithoutTouchingTheLedger(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)

	w.Funding.Fault = func(method, _ string) testsupport.Fault {
		if method == "collect" {
			return testsupport.FaultUnavailable
		}

		return testsupport.FaultNone
	}
	op, err := w.deposit(w.ada, a.ID, 500)
	must(t, err)
	wantStatus(t, op, domain.StatusPending)

	w.recoverAfter(t, time.Hour)
	wantStatus(t, w.operation(t, op.ID), domain.StatusPending)

	w.recoverAfter(t, pastTheDeadline)
	wantFailed(t, w.operation(t, op.ID), domain.FailureTimedOut)
	if n := w.Ledger.CallCount("Credit"); n != 0 {
		t.Errorf("ledger credits = %d: nothing was collected, so nothing may be credited", n)
	}

	rejected := testsupport.FaultRejected
	w.Funding.Fault = func(string, string) testsupport.Fault { return rejected }
	declined, err := w.deposit(w.ada, a.ID, 500)
	must(t, err)
	wantFailed(t, declined, domain.FailureRejected)
}

func TestADepositWhoseMoneyWasCollectedIsNeverFailed(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)

	w.Ledger.Fault = onlyFault("Credit", testsupport.FaultUnavailable)
	op, err := w.deposit(w.ada, a.ID, 500)
	must(t, err)
	wantStatus(t, op, domain.StatusSettling)

	w.recoverAfter(t, pastTheDeadline)
	wantStatus(t, w.operation(t, op.ID), domain.StatusSettling)

	w.Ledger.Fault = nil
	w.recoverAfter(t, time.Hour)
	wantStatus(t, w.operation(t, op.ID), domain.StatusCompleted)
	wantBalance(t, w, a.ID, 500)
}

func TestARefusedDebitFailsTheOperationWithoutARefund(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	// The wallet thought the money was there; by the time the debit ran it was not.
	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultUnavailable)
	op, err := w.transfer(w.ada, a.ID, b.ID, 800)
	must(t, err)
	w.Ledger.Fund(a.ID, -900)
	w.Ledger.Fault = nil

	w.recoverAfter(t, time.Minute)

	failed := w.operation(t, op.ID)
	wantFailed(t, failed, domain.FailureInsufficientFunds)
	if failed.RefundedAt != nil {
		t.Error("nothing was debited, so nothing is refunded")
	}
	wantBalance(t, w, a.ID, 100)
	wantBalance(t, w, b.ID, 0)
}

func TestACreditTheLedgerWillNeverTakeRefundsTheSource(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	w.Ledger.Fault = func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == "Credit" && c.Account == b.ID {
			return testsupport.FaultRejected
		}

		return testsupport.FaultNone
	}
	op, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err)

	failed := w.operation(t, op.ID)
	wantFailed(t, failed, domain.FailureRejected)
	if failed.RefundedAt == nil {
		t.Error("the refund should be recorded")
	}
	wantBalance(t, w, a.ID, 1000)
	wantBalance(t, w, b.ID, 0)
}

func TestAServiceCreditThatIsRefusedOrDelayed(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)

	w.Ledger.Fault = onlyFault("Credit", testsupport.FaultRejected)
	refused, err := w.Svc.Credit(w.ctx, w.service("payment", a.ID, "", 100))
	must(t, err)
	wantFailed(t, refused, domain.FailureRejected)

	w.Ledger.Fault = onlyFault("Credit", testsupport.FaultUnavailable)
	delayed, err := w.Svc.Credit(w.ctx, w.service("payment", a.ID, "", 100))
	must(t, err)
	wantStatus(t, delayed, domain.StatusPending)

	w.Ledger.Fault = nil
	w.recoverAfter(t, time.Minute)
	wantStatus(t, w.operation(t, delayed.ID), domain.StatusCompleted)
	wantBalance(t, w, a.ID, 100)
}

func TestRecoveryLeavesFinishedOperationsAlone(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)
	if _, err := w.transfer(w.ada, a.ID, b.ID, 100); err != nil {
		t.Fatal(err)
	}

	report := w.recoverAfter(t, pastTheDeadline)
	if report.Advanced != 0 || report.Expired != 0 {
		t.Errorf("report = %+v, want nothing to do", report)
	}
	if w.Ledger.CallCount("Debit") != 1 {
		t.Errorf("debits = %d, want 1", w.Ledger.CallCount("Debit"))
	}
}

func TestRandomFaultsNeverCreateOrLoseMoney(t *testing.T) {
	outcomes := map[string]int{}
	for seed := uint64(1); seed <= 40; seed++ {
		runRandomFaults(t, seed, outcomes)
	}

	t.Logf("outcomes over all seeds: %v", outcomes)

	// The test only means something if it drove the failure paths too.
	for _, outcome := range []string{"completed failure_reason(0)", "failed timed_out", "refunded", "debit replayed"} {
		if outcomes[outcome] == 0 {
			t.Errorf("no operation ended as %q: the faults are not exercising that path", outcome)
		}
	}
}

func runRandomFaults(t *testing.T, seed uint64, all map[string]int) {
	t.Helper()

	w := newWorld(t)
	rng := rand.New(rand.NewPCG(seed, 7))

	users := []string{w.ada, w.bob}
	accounts := []string{
		w.account(t, w.ada, "a1", 600).ID, w.account(t, w.ada, "a2", 600).ID,
		w.account(t, w.bob, "b1", 600).ID, w.account(t, w.bob, "b2", 600).ID,
	}
	owner := map[string]string{accounts[0]: users[0], accounts[1]: users[0], accounts[2]: users[1], accounts[3]: users[1]}
	const total = 2400

	w.Ledger.Fault = func(testsupport.LedgerCall) testsupport.Fault {
		switch rng.IntN(10) {
		case 0, 1:
			return testsupport.FaultUnavailable
		case 2:
			return testsupport.FaultLostResponse
		default:
			return testsupport.FaultNone
		}
	}
	w.Store.FailSaveOperation = func(domain.Operation) error {
		if rng.IntN(12) == 0 {
			return errors.New("database went away")
		}

		return nil
	}

	var ids []string
	for i := 0; i < 40; i++ {
		from, to := accounts[rng.IntN(4)], accounts[rng.IntN(4)]
		if from == to {
			continue
		}

		op, err := w.transfer(owner[from], from, to, int64(1+rng.IntN(500)))
		if err == nil {
			ids = append(ids, op.ID)
		}

		switch rng.IntN(6) {
		case 0, 1:
			w.Clock.Advance(30 * time.Hour) // some operations will outlive their deadline
		case 2:
			w.recoverAfter(t, time.Minute)
		}
	}

	// The faults stop; recovery finishes everything.
	w.Ledger.Fault, w.Store.FailSaveOperation = nil, nil
	for round := 0; round < 40; round++ {
		w.recoverAfter(t, time.Hour)
	}

	for _, id := range ids {
		op := w.operation(t, id)
		all[op.Status.String()+" "+op.Failure.String()]++
		if !op.Status.Terminal() {
			t.Fatalf("seed %d: operation %s still %s", seed, id, op.Status)
		}

		debit, debited := w.Ledger.Posted(op.AccountID, op.ID, "debit")
		credit, credited := w.Ledger.Posted(op.CounterpartyID, op.ID, "credit")
		refund, refunded := w.Ledger.Posted(op.AccountID, op.ID, "credit")

		if refunded {
			all["refunded"]++
		}
		if debited && op.Attempts > 1 {
			all["debit replayed"]++ // the debit was repeated by recovery, and applied once
		}

		switch op.Status {
		case domain.StatusCompleted:
			if !debited || !credited || refunded || debit != op.Amount || credit != op.Amount {
				t.Fatalf("seed %d: completed operation %s: debited=%v credited=%v refunded=%v", seed, id, debited, credited, refunded)
			}
		case domain.StatusFailed:
			if credited {
				t.Fatalf("seed %d: failed operation %s still credited the destination", seed, id)
			}
			if debited != refunded || (debited && (refund != op.Amount || op.RefundedAt == nil)) || (!debited && op.RefundedAt != nil) {
				t.Fatalf("seed %d: failed operation %s: debited=%v refunded=%v refundedAt=%v", seed, id, debited, refunded, op.RefundedAt)
			}
		}
	}

	if got := w.Ledger.Total(); got != total {
		t.Fatalf("seed %d: total money = %d, want %d", seed, got, total)
	}
	for _, id := range accounts {
		if got := w.Ledger.BalanceOf(id); got < 0 {
			t.Fatalf("seed %d: account %s overdrawn: %d", seed, id, got)
		}
	}
}

func TestAnExecutorWorkingFromAStaleCopyIsToldWhereTheOperationReallyIs(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)

	// A deposit that stalls on the ledger. The request that made it holds this copy: pending.
	w.Ledger.Fault = onlyFault("Credit", testsupport.FaultUnavailable)
	stale, err := w.deposit(w.ada, a.ID, 500)
	must(t, err)
	wantStatus(t, stale, domain.StatusSettling) // collected, not yet credited

	// Meanwhile recovery finishes it.
	w.Ledger.Fault = nil
	w.recoverAfter(t, time.Minute)
	wantStatus(t, w.operation(t, stale.ID), domain.StatusCompleted)

	// The slow executor now carries on from its stale copy. It must not fail, log an error or hand
	// back the old state: it must come back with the operation as it now stands.
	got := w.Svc.AdvanceForTest(w.ctx, stale)

	wantStatus(t, got, domain.StatusCompleted)
	wantBalance(t, w, a.ID, 500) // and nothing was credited twice
}

func TestAStaleExecutorCannotFailAnOperationThatHasMovedOn(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	// A transfer whose debit stalls: this is the copy the slow executor holds.
	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultUnavailable)
	stale, err := w.transfer(w.ada, a.ID, b.ID, 100)
	must(t, err)
	wantStatus(t, stale, domain.StatusPending)

	// Recovery completes it.
	w.Ledger.Fault = nil
	w.recoverAfter(t, time.Minute)
	wantStatus(t, w.operation(t, stale.ID), domain.StatusCompleted)
	wantBalance(t, w, a.ID, 900)
	wantBalance(t, w, b.ID, 100)

	// The ledger now refuses the stale executor's debit (the balance has changed since), as it
	// would if that executor had a stale view. The completed transfer must not be failed.
	w.Ledger.Fund(a.ID, -900)
	w.Ledger.Fault = func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == "Debit" {
			return testsupport.FaultRejected
		}

		return testsupport.FaultNone
	}

	got := w.Svc.AdvanceForTest(w.ctx, stale)

	wantStatus(t, got, domain.StatusCompleted)
	wantStatus(t, w.operation(t, stale.ID), domain.StatusCompleted)
}
