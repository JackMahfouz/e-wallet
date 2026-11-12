package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

const maxReasonLength = 200

// CreateHold reserves money on an account: until the hold is released, captured or expired, the
// account cannot spend it. It fails if the account cannot spare the amount.
func (s *Service) CreateHold(ctx context.Context, req CreateHoldRequest) (domain.Hold, error) {
	initiator, err := serviceInitiator(req.Service)
	if err != nil {
		return domain.Hold{}, err
	}
	key, err := parseKey(req.IdempotencyKey)
	if err != nil {
		return domain.Hold{}, err
	}
	accountID, err := domain.ParseID("account id", req.AccountID)
	if err != nil {
		return domain.Hold{}, err
	}
	if err := validAmount(req.Amount); err != nil {
		return domain.Hold{}, err
	}
	if req.TTL <= 0 || req.TTL > s.opts.MaxHoldTTL {
		return domain.Hold{}, domain.Invalidf("ttl must be greater than 0 and at most %s", s.opts.MaxHoldTTL)
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) > maxReasonLength {
		return domain.Hold{}, domain.Invalidf("reason must be at most %d characters", maxReasonLength)
	}

	hash := requestHash("hold", accountID, req.Amount, reason, req.TTL)

	var hold domain.Hold

	for attempt := 0; attempt < 2; attempt++ {
		err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
			now := s.Clock.Now()

			existing, err := repo.HoldByKey(ctx, initiator, key)
			switch {
			case err == nil:
				if existing.RequestHash != hash {
					return domain.ErrIdempotencyKeyReused
				}
				hold = existing

				return nil
			case !errors.Is(err, domain.ErrNotFound):
				return err
			}

			account, err := repo.AccountByID(ctx, accountID)
			if err != nil {
				return err
			}
			if _, err := repo.LockWallet(ctx, account.WalletID); err != nil {
				return err
			}
			locked, err := repo.LockAccounts(ctx, accountID)
			if err != nil {
				return err
			}
			if !locked[accountID].CanMove() {
				return fmt.Errorf("%w: the account is %s", domain.ErrAccountNotActive, locked[accountID].Status)
			}

			available, err := s.available(ctx, repo, accountID, now)
			if err != nil {
				return err
			}
			if req.Amount > available {
				return &domain.InsufficientFundsError{Available: available, Requested: req.Amount}
			}

			hold = domain.Hold{
				ID: s.IDs.NewID(), AccountID: accountID, Amount: req.Amount, Reason: reason,
				ExpiresAt: now.Add(req.TTL), Initiator: initiator, IdempotencyKey: key,
				RequestHash: hash, CreatedAt: now,
			}

			return repo.InsertHold(ctx, hold)
		})
		if !errors.Is(err, domain.ErrDuplicateKey) {
			break
		}
	}

	return hold, err
}

// ReleaseHold gives a hold's money back to the account. Only the service that made the hold can
// release it. Releasing twice is fine; releasing a hold that was captured is not.
func (s *Service) ReleaseHold(ctx context.Context, service, holdID string) (domain.Hold, error) {
	initiator, err := serviceInitiator(service)
	if err != nil {
		return domain.Hold{}, err
	}
	id, err := domain.ParseID("hold id", holdID)
	if err != nil {
		return domain.Hold{}, err
	}

	var out domain.Hold

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		hold, err := repo.LockHold(ctx, id)
		if err != nil {
			return err
		}
		if hold.Initiator != initiator {
			return domain.ErrNotFound
		}
		out = hold

		switch {
		case hold.CapturedBy != "":
			return fmt.Errorf("%w: the hold was already captured", domain.ErrConflict)
		case hold.ReleasedAt != nil:
			return nil
		}

		released := s.Clock.Now()
		hold.ReleasedAt = &released
		out = hold

		return repo.SaveHold(ctx, hold)
	})

	return out, err
}

// AccountFor returns any account's figures, for other services.
func (s *Service) AccountFor(ctx context.Context, accountID string) (AccountView, error) {
	id, err := domain.ParseID("account id", accountID)
	if err != nil {
		return AccountView{}, err
	}

	account, err := s.Store.AccountByID(ctx, id)
	if err != nil {
		return AccountView{}, err
	}

	return s.view(ctx, s.Store, account, s.Clock.Now())
}
