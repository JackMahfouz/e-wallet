package application

import (
	"context"
	"errors"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// An operation moves money in up to two legs, and its status records which are done:
//
//	pending   no money has left its source
//	settling  money has left its source (account debited, or funding collected) and not yet arrived
//	completed money is where it should be
//
//	transfer    debit source            -> credit destination
//	withdrawal  debit account           -> funding payout
//	deposit     funding collect         -> credit account
//	debit       debit account
//	credit      credit account
//
// Every ledger call carries the operation id as its transaction id, and the ledger applies each
// (account, transaction, direction) once, so a leg that is repeated (after a crash, a timeout, or
// by recovery racing the request that started it) changes nothing. A refund is a credit to the
// source with the same transaction id: it is a different direction from the debit, so it is
// applied once too.
//
// A leg that fails because a dependency is unreachable is left as it is, and recovery tries again.
// An operation is only failed when that can be done without losing money: a debit that may have
// reached the ledger is refunded first.

// advance drives an operation as far as it will go and returns where it got to. It never returns
// an error: whatever stops it (a dependency down, a database hiccup) leaves the operation where it
// is, and recovery picks it up.
func (s *Service) advance(ctx context.Context, op domain.Operation) domain.Operation {
	for step := 0; step < 4; step++ {
		var (
			next       domain.Operation
			progressed bool
		)

		switch {
		case op.Status == domain.StatusPending && op.Kind.MovesMoneyOut():
			next, progressed = s.debitLeg(ctx, op)
		case op.Status == domain.StatusPending:
			next, progressed = s.firstLeg(ctx, nil, op)
		case op.Status == domain.StatusSettling:
			next, progressed = s.secondLeg(ctx, nil, op)
		default:
			return op
		}

		op = next
		if !progressed {
			return op
		}
	}

	return op
}

func (s *Service) firstLeg(ctx context.Context, repo Repository, op domain.Operation) (domain.Operation, bool) {
	late := op.PastDeadline(s.Clock.Now())

	switch op.Kind {
	case domain.KindCredit:
		_, err := s.Ledger.Credit(ctx, op.AccountID, op.ID, op.Amount)
		switch {
		case err == nil:
			return s.complete(ctx, repo, op)
		case errors.Is(err, domain.ErrRejected):
			return s.fail(ctx, repo, op, domain.FailureRejected)
		default:
			return s.blocked(ctx, op, err)
		}

	case domain.KindDeposit:
		err := s.Funding.Collect(ctx, op.ID, op.Amount)
		switch {
		case err == nil:
			return s.settle(ctx, repo, op)
		case errors.Is(err, domain.ErrRejected):
			return s.fail(ctx, repo, op, domain.FailureRejected)
		case late:
			// Nothing was collected, so nothing was credited: failing loses no money.
			return s.fail(ctx, repo, op, domain.FailureTimedOut)
		default:
			return s.blocked(ctx, op, err)
		}

	default: // the money leaves an account
		_, err := s.Ledger.Debit(ctx, op.AccountID, op.ID, op.Amount)

		var insufficient *domain.InsufficientFundsError

		switch {
		case err == nil && late:
			// The debit is in the ledger (this call or an earlier one), and the operation is out of
			// time: give the money back and fail it.
			return s.refundAndFail(ctx, repo, op, domain.FailureTimedOut)
		case err == nil && op.Kind == domain.KindDebit:
			return s.complete(ctx, repo, op)
		case err == nil:
			return s.settle(ctx, repo, op)
		case errors.As(err, &insufficient):
			return s.fail(ctx, repo, op, domain.FailureInsufficientFunds)
		case errors.Is(err, domain.ErrRejected):
			return s.fail(ctx, repo, op, domain.FailureRejected)
		default:
			return s.blocked(ctx, op, err)
		}
	}
}

func (s *Service) secondLeg(ctx context.Context, repo Repository, op domain.Operation) (domain.Operation, bool) {
	late := op.PastDeadline(s.Clock.Now())

	switch op.Kind {
	case domain.KindTransfer:
		_, err := s.Ledger.Credit(ctx, op.CounterpartyID, op.ID, op.Amount)
		switch {
		case err == nil:
			return s.complete(ctx, repo, op)
		case errors.Is(err, domain.ErrRejected):
			return s.refundAndFail(ctx, repo, op, domain.FailureRejected)
		default:
			return s.blocked(ctx, op, err)
		}

	case domain.KindWithdrawal:
		err := s.Funding.Payout(ctx, op.ID, op.Amount)
		switch {
		case err == nil:
			return s.complete(ctx, repo, op)
		case errors.Is(err, domain.ErrRejected):
			return s.refundAndFail(ctx, repo, op, domain.FailureRejected)
		case late:
			return s.refundAndFail(ctx, repo, op, domain.FailureTimedOut)
		default:
			return s.blocked(ctx, op, err)
		}

	case domain.KindDeposit:
		// The money has been collected, so this cannot be failed; it is retried until the ledger
		// takes it.
		_, err := s.Ledger.Credit(ctx, op.AccountID, op.ID, op.Amount)
		if err == nil {
			return s.complete(ctx, repo, op)
		}

		return s.blocked(ctx, op, err)

	default:
		return s.complete(ctx, repo, op)
	}
}

// refundAndFail credits the debited money back to the source, then fails the operation. If the
// refund cannot be made the operation is left as it is and the refund is retried later.
func (s *Service) refundAndFail(ctx context.Context, repo Repository, op domain.Operation, reason domain.FailureReason) (domain.Operation, bool) {
	if _, err := s.Ledger.Credit(ctx, op.AccountID, op.ID, op.Amount); err != nil {
		return s.blocked(ctx, op, err)
	}

	return s.transition(ctx, repo, op, func(o *domain.Operation) error {
		now := s.Clock.Now()
		o.RefundedAt = &now

		return o.Fail(reason, now)
	})
}

func (s *Service) settle(ctx context.Context, repo Repository, op domain.Operation) (domain.Operation, bool) {
	return s.transition(ctx, repo, op, func(o *domain.Operation) error { return o.Settle(s.Clock.Now()) })
}

func (s *Service) complete(ctx context.Context, repo Repository, op domain.Operation) (domain.Operation, bool) {
	return s.transition(ctx, repo, op, func(o *domain.Operation) error { return o.Complete(s.Clock.Now()) })
}

func (s *Service) fail(ctx context.Context, repo Repository, op domain.Operation, reason domain.FailureReason) (domain.Operation, bool) {
	return s.transition(ctx, repo, op, func(o *domain.Operation) error { return o.Fail(reason, s.Clock.Now()) })
}

// blocked leaves the operation as it is, for recovery to retry, and says why.
func (s *Service) blocked(ctx context.Context, op domain.Operation, cause error) (domain.Operation, bool) {
	level := s.Logger.WarnContext
	if !isTransient(cause) {
		level = s.Logger.ErrorContext
	}
	level(ctx, "operation is waiting to be retried", "operation", op.ID, "kind", op.Kind.String(), "status", op.Status.String(), "error", cause)

	return op, false
}

// transition applies change to the operation if it is still in the status the caller saw. If
// something else (recovery, a retried request) moved it first, it returns the operation as it now
// stands and the caller carries on from there. A failed operation gives back the hold it had
// captured.
func (s *Service) transition(ctx context.Context, repo Repository, op domain.Operation, change func(*domain.Operation) error) (domain.Operation, bool) {
	out := op

	run := func(ctx context.Context, repo Repository) error {
		locked, err := repo.LockOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		out = locked
		if locked.Status != op.Status {
			return nil
		}

		if err := change(&locked); err != nil {
			return err
		}
		if err := repo.SaveOperation(ctx, locked); err != nil {
			return err
		}
		if locked.Status == domain.StatusFailed {
			if err := repo.ReleaseCapture(ctx, locked.ID); err != nil {
				return err
			}
		}
		out = locked

		return nil
	}

	// Inside a transaction the caller holds, the change is part of it; otherwise it has its own.
	var err error
	if repo != nil {
		err = run(ctx, repo)
	} else {
		err = s.Store.Do(ctx, run)
	}
	if errors.Is(err, errNotDue) {
		return out, false // nothing to do, and nothing wrong
	}
	if err != nil {
		// The ledger already did its part; the status could not be recorded. The next attempt
		// repeats the (idempotent) call and records it then.
		return s.blocked(ctx, op, err)
	}

	return out, true
}

// debitLeg runs the first leg of an operation that takes money out of an account: the ledger
// debit. It does so holding the account's row lock, the same one a new operation takes before it
// checks what the account can spend. Without that, the instant between the ledger applying a debit
// and the operation recording it would count the money twice (once as a pending operation, once
// gone from the balance) and refuse a request that could have been met.
//
// Only the account is locked, not the wallet: the wallet lock guards the wallet-wide daily limit,
// and an operation counts towards that from the moment it is pending, whether or not its debit has
// reached the ledger yet. Taking it here would only make debits on different accounts of one wallet
// wait for each other.
//
// The operation's row was committed before this runs, so a crash while the lock is held loses
// nothing: the operation is still pending, and its debit is repeated, harmlessly, by recovery.
func (s *Service) debitLeg(ctx context.Context, op domain.Operation) (domain.Operation, bool) {
	out, progressed := op, false

	err := s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		if _, err := repo.LockAccounts(ctx, op.AccountID); err != nil {
			return err
		}

		current, err := repo.LockOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		if current.Status != op.Status {
			out, progressed = current, true // someone else got there first

			return nil
		}

		out, progressed = s.firstLeg(ctx, repo, current)

		return nil
	})
	if err != nil {
		return s.blocked(ctx, op, err)
	}

	return out, progressed
}

// expireConfirmation fails an awaiting operation whose window has closed, and reports whether it
// was this call that did so. An operation that is not due, or that something else has already
// moved on, is left alone.
func (s *Service) expireConfirmation(ctx context.Context, op domain.Operation) bool {
	changed := false

	_, ok := s.transition(ctx, nil, op, func(o *domain.Operation) error {
		now := s.Clock.Now()
		if !o.Expired(now) {
			return errNotDue
		}
		changed = true

		return o.Fail(domain.FailureConfirmationExpired, now)
	})

	return ok && changed
}

var errNotDue = errors.New("not due")

// Recover is one pass of the recovery job: it fails confirmations that ran out of time, then picks
// up operations that stopped part-way (a dependency was down, the process died) and drives them
// on. It is safe to run in several wallet instances at once.
func (s *Service) Recover(ctx context.Context) (RecoveryReport, error) {
	var report RecoveryReport
	now := s.Clock.Now()

	expired, err := s.Store.DueConfirmations(ctx, now, s.opts.RecoveryBatch)
	if err != nil {
		return report, err
	}
	for _, op := range expired {
		if s.expireConfirmation(ctx, op) {
			report.Expired++
		}
	}

	claimed, err := s.Store.ClaimDue(ctx, now, s.opts.RetryAfter, s.opts.RecoveryBatch)
	if err != nil {
		return report, err
	}
	for _, op := range claimed {
		if ctx.Err() != nil {
			break
		}
		s.advance(ctx, op)
		report.Advanced++
	}

	return report, nil
}
