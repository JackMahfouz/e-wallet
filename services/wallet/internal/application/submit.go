package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// submission is a request to move money, after its callers have checked who may ask for it.
type submission struct {
	kind           domain.OperationKind
	accountID      string
	counterpartyID string
	amount         int64
	holdID         string
	initiator      string
	key            string
}

func (sub submission) hash() string {
	return requestHash(sub.kind, sub.accountID, sub.counterpartyID, sub.amount, sub.holdID)
}

// Deposit funds an account from outside: the funding gateway collects, then the ledger is
// credited.
func (s *Service) Deposit(ctx context.Context, req UserMoneyRequest) (domain.Operation, error) {
	return s.userMoney(ctx, req, domain.KindDeposit)
}

// Withdraw takes money out of an account: the ledger is debited, then the funding gateway pays
// out.
func (s *Service) Withdraw(ctx context.Context, req UserMoneyRequest) (domain.Operation, error) {
	return s.userMoney(ctx, req, domain.KindWithdrawal)
}

func (s *Service) userMoney(ctx context.Context, req UserMoneyRequest, kind domain.OperationKind) (domain.Operation, error) {
	key, err := parseKey(req.IdempotencyKey)
	if err != nil {
		return domain.Operation{}, err
	}
	if err := validAmount(req.Amount); err != nil {
		return domain.Operation{}, err
	}

	_, account, err := s.ownedAccount(ctx, s.Store, req.UserID, req.AccountID)
	if err != nil {
		return domain.Operation{}, err
	}
	if err := s.requireOTP(ctx, req.UserID, req.OTP); err != nil {
		return domain.Operation{}, err
	}
	if !s.opts.FundingEnabled {
		return domain.Operation{}, domain.ErrFundingDisabled
	}

	return s.submit(ctx, submission{
		kind: kind, accountID: account.ID, amount: req.Amount,
		initiator: userInitiator(req.UserID), key: key,
	})
}

// Transfer moves money from one of the user's accounts to any account.
func (s *Service) Transfer(ctx context.Context, req TransferRequest) (domain.Operation, error) {
	key, err := parseKey(req.IdempotencyKey)
	if err != nil {
		return domain.Operation{}, err
	}
	if err := validAmount(req.Amount); err != nil {
		return domain.Operation{}, err
	}
	to, err := domain.ParseID("destination account id", req.ToAccountID)
	if err != nil {
		return domain.Operation{}, err
	}

	_, from, err := s.ownedAccount(ctx, s.Store, req.UserID, req.FromAccountID)
	if err != nil {
		return domain.Operation{}, err
	}
	if from.ID == to {
		return domain.Operation{}, domain.Invalidf("the destination must be a different account")
	}
	if err := s.requireOTP(ctx, req.UserID, req.OTP); err != nil {
		return domain.Operation{}, err
	}

	return s.submit(ctx, submission{
		kind: domain.KindTransfer, accountID: from.ID, counterpartyID: to, amount: req.Amount,
		initiator: userInitiator(req.UserID), key: key,
	})
}

// Credit adds money to an account on behalf of another service.
func (s *Service) Credit(ctx context.Context, req ServiceMoneyRequest) (domain.Operation, error) {
	return s.serviceMoney(ctx, req, domain.KindCredit)
}

// Debit takes money from an account on behalf of another service.
func (s *Service) Debit(ctx context.Context, req ServiceMoneyRequest) (domain.Operation, error) {
	return s.serviceMoney(ctx, req, domain.KindDebit)
}

// TransferBetween moves money between two accounts on behalf of another service.
func (s *Service) TransferBetween(ctx context.Context, req ServiceMoneyRequest) (domain.Operation, error) {
	return s.serviceMoney(ctx, req, domain.KindTransfer)
}

func (s *Service) serviceMoney(ctx context.Context, req ServiceMoneyRequest, kind domain.OperationKind) (domain.Operation, error) {
	initiator, err := serviceInitiator(req.Service)
	if err != nil {
		return domain.Operation{}, err
	}
	key, err := parseKey(req.IdempotencyKey)
	if err != nil {
		return domain.Operation{}, err
	}
	if err := validAmount(req.Amount); err != nil {
		return domain.Operation{}, err
	}
	account, err := domain.ParseID("account id", req.AccountID)
	if err != nil {
		return domain.Operation{}, err
	}

	sub := submission{kind: kind, accountID: account, amount: req.Amount, initiator: initiator, key: key}

	if kind == domain.KindTransfer {
		if sub.counterpartyID, err = domain.ParseID("counterparty id", req.CounterpartyID); err != nil {
			return domain.Operation{}, err
		}
		if sub.counterpartyID == account {
			return domain.Operation{}, domain.Invalidf("the destination must be a different account")
		}
	} else if req.CounterpartyID != "" {
		return domain.Operation{}, domain.Invalidf("only a transfer has a counterparty")
	}

	if req.HoldID != "" {
		if kind == domain.KindCredit {
			return domain.Operation{}, domain.Invalidf("a credit cannot capture a hold")
		}
		if sub.holdID, err = domain.ParseID("hold id", req.HoldID); err != nil {
			return domain.Operation{}, err
		}
	}

	return s.submit(ctx, sub)
}

// submit records the operation and starts it. The record is written first and always: from then
// on, whatever happens to this request, the operation exists and recovery can finish it.
//
// A spend over a daily limit is recorded as awaiting confirmation, and returned together with a
// *domain.ConfirmationRequiredError.
func (s *Service) submit(ctx context.Context, sub submission) (domain.Operation, error) {
	var (
		op      domain.Operation
		confirm *domain.ConfirmationRequiredError
		err     error
	)

	// Two requests with the same key can both miss the lookup; the unique index lets one win and
	// the loser tries again, now finding the winner's row.
	for attempt := 0; attempt < 2; attempt++ {
		op, confirm, err = s.createOperation(ctx, sub)
		if !errors.Is(err, domain.ErrDuplicateKey) {
			break
		}
	}
	if err != nil {
		return domain.Operation{}, err
	}
	if confirm != nil {
		return op, confirm
	}

	return s.advance(ctx, op), nil
}

func (s *Service) createOperation(ctx context.Context, sub submission) (op domain.Operation, confirm *domain.ConfirmationRequiredError, err error) {
	hash := sub.hash()

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		now := s.Clock.Now()

		// A retry of an earlier request returns what that request made.
		existing, err := repo.OperationByKey(ctx, sub.initiator, sub.key)
		switch {
		case err == nil:
			if existing.RequestHash != hash {
				return domain.ErrIdempotencyKeyReused
			}
			if existing.Expired(now) {
				if err := existing.Fail(domain.FailureConfirmationExpired, now); err != nil {
					return err
				}
				if err := repo.SaveOperation(ctx, existing); err != nil {
					return err
				}
			}
			op = existing
			confirm = confirmationFor(existing)

			return nil
		case !errors.Is(err, domain.ErrNotFound):
			return err
		}

		// Lock in the one order every use case uses: wallet, then accounts, then hold.
		first, err := repo.AccountByID(ctx, sub.accountID)
		if err != nil {
			return err
		}
		wallet, err := repo.LockWallet(ctx, first.WalletID)
		if err != nil {
			return err
		}

		ids := []string{sub.accountID}
		if sub.counterpartyID != "" {
			ids = append(ids, sub.counterpartyID)
		}
		accounts, err := repo.LockAccounts(ctx, ids...)
		if err != nil {
			return err
		}

		source := accounts[sub.accountID]
		if !source.CanMove() {
			return fmt.Errorf("%w: the account is %s", domain.ErrAccountNotActive, source.Status)
		}
		sameWallet := true
		if sub.counterpartyID != "" {
			destination := accounts[sub.counterpartyID]
			if !destination.CanMove() {
				return fmt.Errorf("%w: the destination account is %s", domain.ErrAccountNotActive, destination.Status)
			}
			sameWallet = destination.WalletID == source.WalletID
		}

		// Spending is money leaving the wallet; moving it between one's own accounts is not.
		spending := sub.kind.MovesMoneyOut() && !(sub.kind == domain.KindTransfer && sameWallet)

		hold, err := s.lockedHold(ctx, repo, sub, now)
		if err != nil {
			return err
		}

		// Is the money there? A hold already reserved it; otherwise ask the ledger, under the
		// locks, so concurrent requests cannot both spend the same money.
		if sub.kind.MovesMoneyOut() && hold == nil {
			available, err := s.available(ctx, repo, source.ID, now)
			if err != nil {
				return err
			}
			if sub.amount > available {
				return &domain.InsufficientFundsError{Available: available, Requested: sub.amount}
			}
		}

		var scope domain.LimitScope
		if spending {
			if scope, err = s.exceededLimit(ctx, repo, wallet, source, sub.amount, now); err != nil {
				return err
			}
		}

		op = domain.Operation{
			ID: s.IDs.NewID(), Kind: sub.kind, AccountID: sub.accountID, CounterpartyID: sub.counterpartyID,
			Amount: sub.amount, HoldID: sub.holdID, Initiator: sub.initiator, IdempotencyKey: sub.key,
			RequestHash: hash, Spending: spending, CreatedAt: now, UpdatedAt: now,
		}

		if scope != 0 {
			expires := now.Add(s.opts.ConfirmTTL)
			op.Status = domain.StatusAwaitingConfirmation
			op.LimitScope = scope
			op.ConfirmExpiresAt = &expires
			op.DeadlineAt = domain.WorkingDaysAfter(now, s.opts.TimeoutWorkdays)
			op.NextAttemptAt = expires
			confirm = confirmationFor(op)
		} else {
			s.start(&op, now)
		}

		if err := repo.InsertOperation(ctx, op); err != nil {
			return err
		}
		if hold != nil && op.Status == domain.StatusPending {
			hold.CapturedBy = op.ID

			return repo.SaveHold(ctx, *hold)
		}

		return nil
	})

	return op, confirm, err
}

// start makes an operation pending: from here its debit or credit may reach the ledger.
func (s *Service) start(op *domain.Operation, now time.Time) {
	op.Status = domain.StatusPending
	op.ExecutedAt = &now
	op.DeadlineAt = domain.WorkingDaysAfter(now, s.opts.TimeoutWorkdays)
	op.NextAttemptAt = now.Add(s.opts.RetryAfter)
}

// lockedHold loads and locks the hold a debit or transfer names, and checks it can cover the
// amount. It returns nil when no hold is named.
func (s *Service) lockedHold(ctx context.Context, repo Repository, sub submission, now time.Time) (*domain.Hold, error) {
	if sub.holdID == "" {
		return nil, nil
	}

	hold, err := repo.LockHold(ctx, sub.holdID)
	if err != nil {
		return nil, err
	}

	switch {
	case hold.AccountID != sub.accountID:
		return nil, domain.Invalidf("the hold belongs to a different account")
	case !hold.Active(now):
		return nil, fmt.Errorf("%w: the hold is no longer active", domain.ErrConflict)
	case sub.amount > hold.Amount:
		return nil, domain.Invalidf("the amount exceeds the hold")
	}

	return &hold, nil
}

// exceededLimit reports which daily limit, if any, spending amount more would break.
func (s *Service) exceededLimit(ctx context.Context, repo Repository, wallet domain.Wallet, source domain.Account, amount int64, now time.Time) (domain.LimitScope, error) {
	if source.DailyLimit == nil && wallet.DailyLimit == nil {
		return 0, nil
	}

	since := domain.StartOfDay(now)

	accountSpent, err := repo.SpentSince(ctx, source.ID, since)
	if err != nil {
		return 0, err
	}
	if domain.Exceeds(source.DailyLimit, accountSpent, amount) {
		return domain.ScopeAccount, nil
	}

	walletSpent, err := repo.WalletSpentSince(ctx, wallet.ID, since)
	if err != nil {
		return 0, err
	}
	if domain.Exceeds(wallet.DailyLimit, walletSpent, amount) {
		return domain.ScopeWallet, nil
	}

	return 0, nil
}

// confirmationFor is the error that goes with an operation awaiting confirmation, or nil.
func confirmationFor(op domain.Operation) *domain.ConfirmationRequiredError {
	if op.Status != domain.StatusAwaitingConfirmation || op.ConfirmExpiresAt == nil {
		return nil
	}

	return &domain.ConfirmationRequiredError{OperationID: op.ID, Scope: op.LimitScope, ExpiresAt: *op.ConfirmExpiresAt}
}

// Operation returns an operation the user is a party to: the owner of its account, or of a
// transfer's destination.
func (s *Service) Operation(ctx context.Context, userID, operationID string) (domain.Operation, error) {
	id, err := domain.ParseID("operation id", operationID)
	if err != nil {
		return domain.Operation{}, err
	}

	op, err := s.Store.OperationByID(ctx, id)
	if err != nil {
		return domain.Operation{}, err
	}

	if _, _, err := s.ownedAccount(ctx, s.Store, userID, op.AccountID); err == nil {
		return op, nil
	}
	if op.CounterpartyID != "" {
		if _, _, err := s.ownedAccount(ctx, s.Store, userID, op.CounterpartyID); err == nil {
			return op, nil
		}
	}

	return domain.Operation{}, domain.ErrNotFound
}

// OperationFor returns any operation, for other services.
func (s *Service) OperationFor(ctx context.Context, operationID string) (domain.Operation, error) {
	id, err := domain.ParseID("operation id", operationID)
	if err != nil {
		return domain.Operation{}, err
	}

	return s.Store.OperationByID(ctx, id)
}

// Confirm finishes an operation that was waiting because it would exceed a daily limit. It needs
// an explicit acknowledgement and a fresh one-time code, within the confirmation window. The
// funds are checked again: nothing was reserved while it waited.
func (s *Service) Confirm(ctx context.Context, req ConfirmRequest) (domain.Operation, error) {
	if !req.Acknowledged {
		return domain.Operation{}, domain.Invalidf("acknowledge_over_limit must be true to confirm an operation over its limit")
	}

	op, err := s.ownedSource(ctx, req.UserID, req.OperationID)
	if err != nil {
		return domain.Operation{}, err
	}
	if err := s.requireOTP(ctx, req.UserID, req.OTP); err != nil {
		return domain.Operation{}, err
	}

	expired := false

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		now := s.Clock.Now()

		first, err := repo.AccountByID(ctx, op.AccountID)
		if err != nil {
			return err
		}
		if _, err := repo.LockWallet(ctx, first.WalletID); err != nil {
			return err
		}

		ids := []string{op.AccountID}
		if op.CounterpartyID != "" {
			ids = append(ids, op.CounterpartyID)
		}
		accounts, err := repo.LockAccounts(ctx, ids...)
		if err != nil {
			return err
		}

		hold, err := s.confirmHold(ctx, repo, op, now)
		if err != nil {
			return err
		}

		locked, err := repo.LockOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		op = locked

		if op.Status == domain.StatusFailed {
			return fmt.Errorf("%w: the operation already failed (%s)", domain.ErrConflict, op.Failure)
		}
		if op.Status != domain.StatusAwaitingConfirmation {
			return nil // already confirmed and under way or done: confirming again is idempotent
		}
		if op.Expired(now) {
			expired = true
			if err := op.Fail(domain.FailureConfirmationExpired, now); err != nil {
				return err
			}

			return repo.SaveOperation(ctx, op)
		}

		if !accounts[op.AccountID].CanMove() {
			return fmt.Errorf("%w: the account is %s", domain.ErrAccountNotActive, accounts[op.AccountID].Status)
		}
		if op.CounterpartyID != "" && !accounts[op.CounterpartyID].CanMove() {
			return fmt.Errorf("%w: the destination account is %s", domain.ErrAccountNotActive, accounts[op.CounterpartyID].Status)
		}

		if hold == nil {
			available, err := s.available(ctx, repo, op.AccountID, now)
			if err != nil {
				return err
			}
			if op.Amount > available {
				return &domain.InsufficientFundsError{Available: available, Requested: op.Amount}
			}
		}

		if err := op.Confirm(now); err != nil {
			return err
		}
		s.start(&op, now)
		op.OverLimitConfirmed = true
		if err := repo.SaveOperation(ctx, op); err != nil {
			return err
		}
		if hold != nil {
			hold.CapturedBy = op.ID

			return repo.SaveHold(ctx, *hold)
		}

		return nil
	})
	if err != nil {
		return domain.Operation{}, err
	}
	if expired {
		return op, fmt.Errorf("%w: the confirmation window has closed", domain.ErrConflict)
	}

	return s.advance(ctx, op), nil
}

// confirmHold locks the hold an awaiting operation names, if it names one, and checks it still
// covers the amount. For any other state of the operation it does nothing.
func (s *Service) confirmHold(ctx context.Context, repo Repository, op domain.Operation, now time.Time) (*domain.Hold, error) {
	if op.HoldID == "" || op.Status != domain.StatusAwaitingConfirmation {
		return nil, nil
	}

	return s.lockedHold(ctx, repo, submission{accountID: op.AccountID, holdID: op.HoldID, amount: op.Amount}, now)
}

// Cancel abandons an operation that is awaiting confirmation. Nothing had moved, so nothing needs
// undoing, and no one-time code is needed.
func (s *Service) Cancel(ctx context.Context, userID, operationID string) (domain.Operation, error) {
	op, err := s.ownedSource(ctx, userID, operationID)
	if err != nil {
		return domain.Operation{}, err
	}

	var out domain.Operation

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		locked, err := repo.LockOperation(ctx, op.ID)
		if err != nil {
			return err
		}
		out = locked

		switch {
		case locked.Status == domain.StatusFailed && locked.Failure == domain.FailureCancelled:
			return nil // cancelling is idempotent
		case locked.Status != domain.StatusAwaitingConfirmation:
			return fmt.Errorf("%w: only an operation awaiting confirmation can be cancelled (this one is %s)", domain.ErrConflict, locked.Status)
		}

		if err := locked.Fail(domain.FailureCancelled, s.Clock.Now()); err != nil {
			return err
		}
		out = locked

		return repo.SaveOperation(ctx, locked)
	})

	return out, err
}

// ownedSource loads an operation whose source account (the one it is about) belongs to the user.
func (s *Service) ownedSource(ctx context.Context, userID, operationID string) (domain.Operation, error) {
	id, err := domain.ParseID("operation id", operationID)
	if err != nil {
		return domain.Operation{}, err
	}

	op, err := s.Store.OperationByID(ctx, id)
	if err != nil {
		return domain.Operation{}, err
	}
	if _, _, err := s.ownedAccount(ctx, s.Store, userID, op.AccountID); err != nil {
		return domain.Operation{}, err
	}

	return op, nil
}

func parseKey(key string) (string, error) {
	if key == "" {
		return "", domain.Invalidf("an Idempotency-Key (a UUID) is required")
	}

	return domain.ParseID("idempotency key", key)
}

func validAmount(cents int64) error {
	if cents <= 0 {
		return domain.Invalidf("the amount must be greater than 0")
	}

	return nil
}
