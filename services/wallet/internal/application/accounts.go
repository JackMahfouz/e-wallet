package application

import (
	"context"
	"fmt"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// Wallet returns the user's wallet, making it on first use.
func (s *Service) Wallet(ctx context.Context, userID string) (WalletView, error) {
	wallet, err := s.ensureWallet(ctx, s.Store, userID)
	if err != nil {
		return WalletView{}, err
	}

	return s.walletView(ctx, wallet)
}

func (s *Service) walletView(ctx context.Context, wallet domain.Wallet) (WalletView, error) {
	now := s.Clock.Now()

	accounts, err := s.Store.AccountsByWallet(ctx, wallet.ID)
	if err != nil {
		return WalletView{}, err
	}

	spent, err := s.Store.WalletSpentSince(ctx, wallet.ID, domain.StartOfDay(now))
	if err != nil {
		return WalletView{}, err
	}

	out := WalletView{Wallet: wallet, SpentToday: spent, Accounts: make([]AccountView, 0, len(accounts))}
	for _, account := range accounts {
		view, err := s.view(ctx, s.Store, account, now)
		if err != nil {
			return WalletView{}, err
		}
		out.Accounts = append(out.Accounts, view)
	}

	return out, nil
}

// SetWalletLimit sets or clears the wallet's daily limit. The accounts' limits, the ones that are
// set, may not add up to more than it.
func (s *Service) SetWalletLimit(ctx context.Context, userID, otp string, change LimitChange) (WalletView, error) {
	wallet, err := s.ensureWallet(ctx, s.Store, userID)
	if err != nil {
		return WalletView{}, err
	}
	if !change.Set {
		return s.walletView(ctx, wallet)
	}

	limit, err := validLimit(change.Value)
	if err != nil {
		return WalletView{}, err
	}
	if err := s.requireOTP(ctx, userID, otp); err != nil {
		return WalletView{}, err
	}

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		locked, err := repo.LockWallet(ctx, wallet.ID)
		if err != nil {
			return err
		}

		accounts, err := repo.AccountsByWallet(ctx, locked.ID)
		if err != nil {
			return err
		}
		if !domain.LimitSumOK(limit, openLimits(accounts, "")) {
			return fmt.Errorf("%w: the accounts' limits already add up to more than that", domain.ErrLimitSum)
		}

		locked.DailyLimit = limit
		locked.UpdatedAt = s.Clock.Now()

		return repo.SaveWallet(ctx, locked)
	})
	if err != nil {
		return WalletView{}, err
	}

	return s.Wallet(ctx, userID)
}

// CreateAccount adds an account to the user's wallet.
func (s *Service) CreateAccount(ctx context.Context, userID, name string, limit *int64) (AccountView, error) {
	name, err := domain.ParseName(name)
	if err != nil {
		return AccountView{}, err
	}
	limit, err = validLimit(limit)
	if err != nil {
		return AccountView{}, err
	}

	var created domain.Account

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		wallet, err := s.ensureWallet(ctx, repo, userID)
		if err != nil {
			return err
		}
		if wallet, err = repo.LockWallet(ctx, wallet.ID); err != nil {
			return err
		}

		accounts, err := repo.AccountsByWallet(ctx, wallet.ID)
		if err != nil {
			return err
		}
		if open := len(openLimits(accounts, "")); open >= s.opts.MaxAccounts {
			return fmt.Errorf("%w: a wallet may have at most %d open accounts", domain.ErrConflict, s.opts.MaxAccounts)
		}
		if !domain.LimitSumOK(wallet.DailyLimit, append(openLimits(accounts, ""), limit)) {
			return fmt.Errorf("%w: it would take the accounts' limits over the wallet limit", domain.ErrLimitSum)
		}

		now := s.Clock.Now()
		created = domain.Account{
			ID: s.IDs.NewID(), WalletID: wallet.ID, Name: name, Status: domain.AccountActive,
			DailyLimit: limit, CreatedAt: now, UpdatedAt: now,
		}

		return repo.InsertAccount(ctx, created)
	})
	if err != nil {
		return AccountView{}, err
	}

	return s.view(ctx, s.Store, created, s.Clock.Now())
}

// Account returns one of the user's accounts.
func (s *Service) Account(ctx context.Context, userID, accountID string) (AccountView, error) {
	_, account, err := s.ownedAccount(ctx, s.Store, userID, accountID)
	if err != nil {
		return AccountView{}, err
	}

	return s.view(ctx, s.Store, account, s.Clock.Now())
}

// UpdateAccount renames an account, sets or clears its limit, or freezes or unfreezes it. A limit
// change and an unfreeze need a one-time code; the safe directions (rename, freeze) do not.
func (s *Service) UpdateAccount(ctx context.Context, userID, accountID, otp string, patch AccountPatch) (AccountView, error) {
	_, account, err := s.ownedAccount(ctx, s.Store, userID, accountID)
	if err != nil {
		return AccountView{}, err
	}

	var name string
	if patch.Name != nil {
		if name, err = domain.ParseName(*patch.Name); err != nil {
			return AccountView{}, err
		}
	}

	limit := patch.Limit.Value
	if patch.Limit.Set {
		if limit, err = validLimit(limit); err != nil {
			return AccountView{}, err
		}
	}

	if patch.Status != nil && *patch.Status != domain.AccountActive && *patch.Status != domain.AccountFrozen {
		return AccountView{}, domain.Invalidf("status can only be set to active or frozen; close an account by deleting it")
	}

	unfreezing := patch.Status != nil && *patch.Status == domain.AccountActive && account.Status == domain.AccountFrozen
	if patch.Limit.Set || unfreezing {
		if err := s.requireOTP(ctx, userID, otp); err != nil {
			return AccountView{}, err
		}
	}

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		wallet, err := repo.LockWallet(ctx, account.WalletID)
		if err != nil {
			return err
		}

		locked, err := repo.LockAccounts(ctx, account.ID)
		if err != nil {
			return err
		}
		current := locked[account.ID]
		if current.Status == domain.AccountClosed {
			return fmt.Errorf("%w: the account is closed", domain.ErrConflict)
		}

		if patch.Name != nil {
			current.Name = name
		}
		if patch.Status != nil {
			current.Status = *patch.Status
		}
		if patch.Limit.Set {
			accounts, err := repo.AccountsByWallet(ctx, wallet.ID)
			if err != nil {
				return err
			}
			if !domain.LimitSumOK(wallet.DailyLimit, append(openLimits(accounts, current.ID), limit)) {
				return fmt.Errorf("%w: it would take the accounts' limits over the wallet limit", domain.ErrLimitSum)
			}
			current.DailyLimit = limit
		}

		current.UpdatedAt = s.Clock.Now()

		return repo.SaveAccount(ctx, current)
	})
	if err != nil {
		return AccountView{}, err
	}

	return s.Account(ctx, userID, accountID)
}

// CloseAccount closes an account for good. It needs an empty ledger balance, no active hold and
// nothing in flight.
func (s *Service) CloseAccount(ctx context.Context, userID, accountID, otp string) (AccountView, error) {
	_, account, err := s.ownedAccount(ctx, s.Store, userID, accountID)
	if err != nil {
		return AccountView{}, err
	}
	if err := s.requireOTP(ctx, userID, otp); err != nil {
		return AccountView{}, err
	}

	err = s.Store.Do(ctx, func(ctx context.Context, repo Repository) error {
		if _, err := repo.LockWallet(ctx, account.WalletID); err != nil {
			return err
		}

		locked, err := repo.LockAccounts(ctx, account.ID)
		if err != nil {
			return err
		}
		current := locked[account.ID]
		if current.Status == domain.AccountClosed {
			return nil // closing is idempotent
		}

		now := s.Clock.Now()

		if n, err := repo.InFlightCount(ctx, current.ID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: %d operation(s) on this account are still in flight", domain.ErrConflict, n)
		}
		if n, err := repo.ActiveHoldCount(ctx, current.ID, now); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: the account has an active hold", domain.ErrConflict)
		}

		balance, err := s.Ledger.Balance(ctx, current.ID)
		if err != nil {
			return err
		}
		if balance != 0 {
			return fmt.Errorf("%w: the balance must be zero to close the account", domain.ErrConflict)
		}

		current.Status = domain.AccountClosed
		current.ClosedAt = &now
		current.UpdatedAt = now

		return repo.SaveAccount(ctx, current)
	})
	if err != nil {
		return AccountView{}, err
	}

	return s.Account(ctx, userID, accountID)
}

// Transactions lists the operations touching one of the user's accounts, newest first.
func (s *Service) Transactions(ctx context.Context, userID, accountID string, before *OperationCursor, limit int) ([]domain.Operation, error) {
	_, account, err := s.ownedAccount(ctx, s.Store, userID, accountID)
	if err != nil {
		return nil, err
	}

	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}

	return s.Store.ListAccountOperations(ctx, account.ID, before, limit)
}

// openLimits returns the limits of the wallet's open accounts (nil for an account without one),
// leaving out the account with id skip.
func openLimits(accounts []domain.Account, skip string) []*int64 {
	limits := make([]*int64, 0, len(accounts))
	for _, account := range accounts {
		if account.Status != domain.AccountClosed && account.ID != skip {
			limits = append(limits, account.DailyLimit)
		}
	}

	return limits
}

// validLimit checks an optional limit.
func validLimit(limit *int64) (*int64, error) {
	if limit == nil {
		return nil, nil
	}

	value, err := domain.ParseLimit(*limit)
	if err != nil {
		return nil, err
	}

	return &value, nil
}
