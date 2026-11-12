package application_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

func TestAuthenticate(t *testing.T) {
	w := newWorld(t)
	w.Tokens.Add("good", w.ada)
	w.Tokens.Add("legacy", "42") // a token whose subject is an old integer id

	if user, err := w.Svc.Authenticate(w.ctx, "good"); err != nil || user != w.ada {
		t.Fatalf("Authenticate = %q, %v", user, err)
	}
	for name, token := range map[string]string{"unknown": "nope", "empty": "", "blank": "   ", "legacy integer subject": "legacy"} {
		if _, err := w.Svc.Authenticate(w.ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", name, err)
		}
	}

	w.Tokens.Down = true
	if _, err := w.Svc.Authenticate(w.ctx, "good"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("auth service down: err = %v, want ErrUnavailable", err)
	}
}

func TestWalletIsCreatedOnFirstUseAndIsStable(t *testing.T) {
	w := newWorld(t)

	first, err := w.Svc.Wallet(w.ctx, w.ada)
	must(t, err)
	second, err := w.Svc.Wallet(w.ctx, w.ada)
	must(t, err)
	other, err := w.Svc.Wallet(w.ctx, w.bob)
	must(t, err)

	if first.ID != second.ID || first.UserID != w.ada || first.ID == other.ID {
		t.Errorf("wallets: %q %q %q", first.ID, second.ID, other.ID)
	}
	if len(first.Accounts) != 0 || first.DailyLimit != nil {
		t.Errorf("a new wallet is empty and unlimited: %+v", first)
	}
}

func TestAccountsLiveInOneWalletAndShowTheirFigures(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "Current", 1000)
	b := w.account(t, w.ada, "Savings", 0)

	wallet, err := w.Svc.Wallet(w.ctx, w.ada)
	must(t, err)

	if len(wallet.Accounts) != 2 || wallet.Accounts[0].ID != a.ID || wallet.Accounts[1].ID != b.ID {
		t.Fatalf("accounts = %+v", wallet.Accounts)
	}
	if got := wallet.Accounts[0]; got.Balance != 1000 || got.Available != 1000 || got.Held != 0 || got.Name != "Current" || got.Status != domain.AccountActive {
		t.Errorf("figures = %+v", got)
	}
	if a.WalletID != b.WalletID {
		t.Error("both accounts belong to the user's one wallet")
	}
}

func TestCreateAccountValidatesAndCapsTheCount(t *testing.T) {
	w := newWorld(t, func(o *application.Options) { o.MaxAccounts = 2 })

	for name, bad := range map[string]string{"empty": "", "too long": strings.Repeat("x", 65)} {
		if _, err := w.Svc.CreateAccount(w.ctx, w.ada, bad, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s name: %v", name, err)
		}
	}
	if _, err := w.Svc.CreateAccount(w.ctx, w.ada, "x", i64(0)); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a zero limit is invalid: %v", err)
	}

	first := w.account(t, w.ada, "one", 0)
	w.account(t, w.ada, "two", 0)
	_, err := w.Svc.CreateAccount(w.ctx, w.ada, "three", nil)
	is(t, err, domain.ErrConflict)

	// Closing an account makes room: only open accounts count.
	_, err = w.Svc.CloseAccount(w.ctx, w.ada, first.ID, testsupport.ValidOTP)
	must(t, err)
	w.account(t, w.ada, "three", 0)

	// The cap is per wallet.
	w.account(t, w.bob, "bob's", 0)
}

func TestAccountLimitsMustFitUnderTheWalletLimit(t *testing.T) {
	w := newWorld(t)
	otp := testsupport.ValidOTP

	_, err := w.Svc.SetWalletLimit(w.ctx, w.ada, otp, application.LimitChange{Set: true, Value: i64(100)})
	must(t, err)

	a := w.limited(t, w.ada, "a", 0, 60)
	_, err = w.Svc.CreateAccount(w.ctx, w.ada, "b", i64(50))
	is(t, err, domain.ErrLimitSum) // 60 + 50 > 100

	b, err := w.Svc.CreateAccount(w.ctx, w.ada, "b", i64(40))
	must(t, err) // 60 + 40 = 100 is allowed

	// Raising an account limit is checked against the others.
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, b.ID, otp, application.AccountPatch{Limit: application.LimitChange{Set: true, Value: i64(41)}})
	is(t, err, domain.ErrLimitSum)
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, b.ID, otp, application.AccountPatch{Limit: application.LimitChange{Set: true, Value: i64(30)}})
	must(t, err)

	// Lowering the wallet limit below the sum is refused; to the sum is fine.
	_, err = w.Svc.SetWalletLimit(w.ctx, w.ada, otp, application.LimitChange{Set: true, Value: i64(89)})
	is(t, err, domain.ErrLimitSum)
	_, err = w.Svc.SetWalletLimit(w.ctx, w.ada, otp, application.LimitChange{Set: true, Value: i64(90)})
	must(t, err)

	// An account with no limit of its own is bounded only by the wallet limit.
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, b.ID, otp, application.AccountPatch{Limit: application.LimitChange{Set: true}})
	must(t, err)

	// A closed account's limit no longer counts.
	_, err = w.Svc.CloseAccount(w.ctx, w.ada, a.ID, otp)
	must(t, err)
	_, err = w.Svc.SetWalletLimit(w.ctx, w.ada, otp, application.LimitChange{Set: true, Value: i64(1)})
	must(t, err)

	// A wallet limit can be cleared, after which nothing is checked.
	wallet, err := w.Svc.SetWalletLimit(w.ctx, w.ada, otp, application.LimitChange{Set: true})
	must(t, err)
	if wallet.DailyLimit != nil {
		t.Errorf("limit = %v, want cleared", *wallet.DailyLimit)
	}
	if _, err := w.Svc.CreateAccount(w.ctx, w.ada, "big", i64(1_000_000)); err != nil {
		t.Errorf("with no wallet limit any account limit fits: %v", err)
	}
}

func TestAnotherUsersAccountIsNotFoundEverywhere(t *testing.T) {
	w := newWorld(t)
	mine := w.account(t, w.ada, "mine", 100)
	w.account(t, w.bob, "bobs", 0)
	otp := testsupport.ValidOTP

	calls := map[string]func() error{
		"Account": func() error { _, err := w.Svc.Account(w.ctx, w.bob, mine.ID); return err },
		"UpdateAccount": func() error {
			_, err := w.Svc.UpdateAccount(w.ctx, w.bob, mine.ID, otp, application.AccountPatch{Name: new(string)})
			return err
		},
		"CloseAccount":  func() error { _, err := w.Svc.CloseAccount(w.ctx, w.bob, mine.ID, otp); return err },
		"Transactions":  func() error { _, err := w.Svc.Transactions(w.ctx, w.bob, mine.ID, nil, 10); return err },
		"Deposit":       func() error { _, err := w.deposit(w.bob, mine.ID, 5); return err },
		"Withdraw":      func() error { _, err := w.withdraw(w.bob, mine.ID, 5); return err },
		"Transfer from": func() error { _, err := w.transfer(w.bob, mine.ID, w.account(t, w.bob, "x", 0).ID, 5); return err },
		"stranger":      func() error { _, err := w.Svc.Account(w.ctx, w.stranger, mine.ID); return err },
	}

	for name, call := range calls {
		if err := call(); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	wantBalance(t, w, mine.ID, 100)
}

func TestSensitiveActionsNeedTheOTPAndSafeOnesDoNot(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)
	otp := testsupport.ValidOTP

	// Safe: create (above), rename, freeze. No code at all.
	name := "renamed"
	_, err := w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, "", application.AccountPatch{Name: &name})
	must(t, err)
	frozen := domain.AccountFrozen
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, "", application.AccountPatch{Status: &frozen})
	must(t, err)

	// Sensitive: each refuses a missing or wrong code and changes nothing.
	active := domain.AccountActive
	limit := application.LimitChange{Set: true, Value: i64(5)}
	for _, code := range []string{"", "000000"} {
		for call, run := range map[string]func() error{
			"unfreeze": func() error {
				_, err := w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, code, application.AccountPatch{Status: &active})
				return err
			},
			"account limit": func() error {
				_, err := w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, code, application.AccountPatch{Limit: limit})
				return err
			},
			"wallet limit": func() error { _, err := w.Svc.SetWalletLimit(w.ctx, w.ada, code, limit); return err },
			"close":        func() error { _, err := w.Svc.CloseAccount(w.ctx, w.ada, a.ID, code); return err },
			"deposit": func() error {
				_, err := w.Svc.Deposit(w.ctx, application.UserMoneyRequest{UserID: w.ada, AccountID: a.ID, Amount: 5, IdempotencyKey: key(), OTP: code})
				return err
			},
			"withdraw": func() error {
				_, err := w.Svc.Withdraw(w.ctx, application.UserMoneyRequest{UserID: w.ada, AccountID: a.ID, Amount: 5, IdempotencyKey: key(), OTP: code})
				return err
			},
			"transfer": func() error {
				_, err := w.Svc.Transfer(w.ctx, application.TransferRequest{UserID: w.ada, FromAccountID: a.ID, ToAccountID: testsupport.UUID(9), Amount: 5, IdempotencyKey: key(), OTP: code})
				return err
			},
			"confirm": func() error {
				_, err := w.Svc.Confirm(w.ctx, application.ConfirmRequest{UserID: w.ada, OperationID: testsupport.UUID(9), OTP: code, Acknowledged: true})
				return err
			},
		} {
			err := run()
			if call == "confirm" {
				is(t, err, domain.ErrNotFound) // ownership is checked before the code, so nothing leaks
				continue
			}
			if !errors.Is(err, domain.ErrOTPInvalid) {
				t.Errorf("%s with code %q: err = %v, want ErrOTPInvalid", call, code, err)
			}
		}
	}

	got := w.view(t, w.ada, a.ID)
	if got.Status != domain.AccountFrozen || got.DailyLimit != nil || got.Name != "renamed" {
		t.Errorf("a refused action changed the account: %+v", got)
	}

	// With the code they work. Unfreezing an account that is not frozen needs none.
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, otp, application.AccountPatch{Status: &active})
	must(t, err)
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, "", application.AccountPatch{Status: &active})
	must(t, err)
}

func TestUpdateAccountRejectsBadPatches(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 0)
	otp := testsupport.ValidOTP

	closed := domain.AccountClosed
	empty := " "
	for name, patch := range map[string]application.AccountPatch{
		"closing by patch": {Status: &closed},
		"blank name":       {Name: &empty},
		"zero limit":       {Limit: application.LimitChange{Set: true, Value: i64(0)}},
	} {
		if _, err := w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, otp, patch); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	_, err := w.Svc.CloseAccount(w.ctx, w.ada, a.ID, otp)
	must(t, err)
	name := "x"
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, a.ID, otp, application.AccountPatch{Name: &name})
	is(t, err, domain.ErrConflict) // a closed account is final
}

func TestFrozenAndClosedAccountsCannotSendOrReceive(t *testing.T) {
	w := newWorld(t)
	src := w.account(t, w.ada, "src", 500)
	dst := w.account(t, w.bob, "dst", 0)
	otp := testsupport.ValidOTP

	frozen := domain.AccountFrozen
	_, err := w.Svc.UpdateAccount(w.ctx, w.ada, src.ID, "", application.AccountPatch{Status: &frozen})
	must(t, err)

	_, err = w.transfer(w.ada, src.ID, dst.ID, 10)
	is(t, err, domain.ErrAccountNotActive) // cannot send
	_, err = w.withdraw(w.ada, src.ID, 10)
	is(t, err, domain.ErrAccountNotActive)
	_, err = w.deposit(w.ada, src.ID, 10)
	is(t, err, domain.ErrAccountNotActive)
	_, err = w.Svc.Credit(w.ctx, w.service("payment", src.ID, "", 10))
	is(t, err, domain.ErrAccountNotActive) // a service cannot credit it either

	// Nor can anyone send to it.
	other := w.account(t, w.bob, "other", 500)
	_, err = w.transfer(w.bob, other.ID, src.ID, 10)
	is(t, err, domain.ErrAccountNotActive)
	wantBalance(t, w, src.ID, 500)
	wantBalance(t, w, other.ID, 500)

	// Unfrozen, it works again; closed, it never does.
	active := domain.AccountActive
	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, src.ID, otp, application.AccountPatch{Status: &active})
	must(t, err)
	op, err := w.transfer(w.ada, src.ID, dst.ID, 10)
	must(t, err)
	wantStatus(t, op, domain.StatusCompleted)

	_, err = w.Svc.UpdateAccount(w.ctx, w.ada, src.ID, otp, application.AccountPatch{Status: &frozen})
	must(t, err)
	_, err = w.Svc.CloseAccount(w.ctx, w.bob, dst.ID, otp) // dst has 10, so it cannot close yet
	is(t, err, domain.ErrConflict)
}

func TestClosingNeedsAnEmptyQuietAccount(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 100)
	otp := testsupport.ValidOTP

	_, err := w.Svc.CloseAccount(w.ctx, w.ada, a.ID, otp)
	is(t, err, domain.ErrConflict)
	if !strings.Contains(err.Error(), "balance") {
		t.Errorf("err = %v, want it to mention the balance", err)
	}

	// An active hold blocks it.
	hold, err := w.Svc.CreateHold(w.ctx, application.CreateHoldRequest{Service: "payment", AccountID: a.ID, Amount: 40, TTL: time.Hour, IdempotencyKey: key()})
	must(t, err)
	_, err = w.Svc.CloseAccount(w.ctx, w.ada, a.ID, otp)
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "hold") {
		t.Errorf("err = %v, want a conflict about the hold", err)
	}
	_, err = w.Svc.ReleaseHold(w.ctx, "payment", hold.ID)
	must(t, err)

	// So does an operation still in flight, here a withdrawal stuck waiting for the ledger.
	w.Ledger.Fault = onlyFault("Debit", testsupport.FaultUnavailable)
	op, err := w.withdraw(w.ada, a.ID, 100)
	must(t, err)
	wantStatus(t, op, domain.StatusPending)
	_, err = w.Svc.CloseAccount(w.ctx, w.ada, a.ID, otp)
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), "in flight") {
		t.Errorf("err = %v, want a conflict about the operation in flight", err)
	}

	// Once the withdrawal has finished and the balance is zero, it closes, and closing twice is fine.
	w.Ledger.Fault = nil
	w.recoverAfter(t, time.Minute)
	wantStatus(t, w.operation(t, op.ID), domain.StatusCompleted)

	closed, err := w.Svc.CloseAccount(w.ctx, w.ada, a.ID, otp)
	must(t, err)
	if closed.Status != domain.AccountClosed || closed.ClosedAt == nil {
		t.Errorf("closed account = %+v", closed.Account)
	}
	if _, err := w.Svc.CloseAccount(w.ctx, w.ada, a.ID, otp); err != nil {
		t.Errorf("closing again: %v", err)
	}
	_, err = w.deposit(w.ada, a.ID, 5)
	is(t, err, domain.ErrAccountNotActive)
}

func TestTransactionsArePagedNewestFirst(t *testing.T) {
	w := newWorld(t)
	a := w.account(t, w.ada, "a", 1000)
	b := w.account(t, w.bob, "b", 0)

	var ids []string
	for i := 0; i < 5; i++ {
		w.Clock.Advance(time.Second)
		op, err := w.transfer(w.ada, a.ID, b.ID, 10)
		must(t, err)
		ids = append([]string{op.ID}, ids...) // newest first
	}

	var got []string
	var cursor *application.OperationCursor
	for pages := 0; pages < 5; pages++ {
		page, err := w.Svc.Transactions(w.ctx, w.ada, a.ID, cursor, 2)
		must(t, err)
		if len(page) == 0 {
			break
		}
		for _, op := range page {
			got = append(got, op.ID)
		}
		last := page[len(page)-1]
		cursor = &application.OperationCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("order = %v, want %v", got, ids)
	}

	// The recipient sees the same operations on their side; a stranger sees nothing of the account.
	theirs, err := w.Svc.Transactions(w.ctx, w.bob, b.ID, nil, 0)
	must(t, err)
	if len(theirs) != 5 {
		t.Errorf("recipient sees %d operations, want 5", len(theirs))
	}
}
