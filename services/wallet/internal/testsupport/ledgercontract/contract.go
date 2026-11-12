// Package ledgercontract is one suite of tests for application.Ledger, run against the in-memory
// fake, against the wallet's gRPC client over a fake server, and against the real ledger service
// when WALLET_TEST_LEDGER_ADDR points at one. The fake is only trustworthy if the real thing
// passes the same tests.
package ledgercontract

import (
	"context"
	"errors"
	"testing"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

var ctx = context.Background()

// Run runs the suite against the Ledger that newLedger returns. Accounts and transactions use
// fresh random ids, so a ledger that is not empty (a real one) is fine.
func Run(t *testing.T, newLedger func(t *testing.T) application.Ledger) {
	suites := map[string]func(*testing.T, application.Ledger){
		"an unknown account has no money":               unknownAccount,
		"credits add and are applied once":              credits,
		"debits subtract, never below zero, once":       debits,
		"a transaction id cannot change its amount":     reusedTransaction,
		"a debit and its refund share a transaction id": refund,
		"amounts survive the trip to cents and back":    amounts,
	}

	for name, suite := range suites {
		t.Run(name, func(t *testing.T) { suite(t, newLedger(t)) })
	}
}

func id(t *testing.T) string {
	t.Helper()

	return testsupport.NewRandomUUID()
}

func must(t testing.TB, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func balance(t *testing.T, l application.Ledger, account string) int64 {
	t.Helper()

	cents, err := l.Balance(ctx, account)
	must(t, err)

	return cents
}

func unknownAccount(t *testing.T, l application.Ledger) {
	if got := balance(t, l, id(t)); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}

	var insufficient *domain.InsufficientFundsError
	_, err := l.Debit(ctx, id(t), id(t), 1)
	if !errors.As(err, &insufficient) {
		t.Errorf("debit of an empty account: %v, want insufficient funds", err)
	}
}

func credits(t *testing.T, l application.Ledger) {
	account, tx := id(t), id(t)

	posting, err := l.Credit(ctx, account, tx, 1250)
	must(t, err)
	if posting.Replayed {
		t.Error("a first credit is not a replay")
	}
	if got := balance(t, l, account); got != 1250 {
		t.Errorf("balance = %d, want 1250", got)
	}

	posting, err = l.Credit(ctx, account, tx, 1250) // a retry
	must(t, err)
	if !posting.Replayed {
		t.Error("a repeated credit should say it was replayed")
	}
	if got := balance(t, l, account); got != 1250 {
		t.Errorf("balance after the retry = %d, want 1250", got)
	}
}

func debits(t *testing.T, l application.Ledger) {
	account, tx := id(t), id(t)
	_, err := l.Credit(ctx, account, id(t), 1000)
	must(t, err)

	var insufficient *domain.InsufficientFundsError
	_, err = l.Debit(ctx, account, id(t), 1001)
	if !errors.As(err, &insufficient) {
		t.Fatalf("overdraft: %v, want insufficient funds", err)
	}
	if insufficient.Requested != 1001 || insufficient.Available != 1000 {
		t.Errorf("insufficient = %+v, want available 1000 and requested 1001", insufficient)
	}

	posting, err := l.Debit(ctx, account, tx, 400)
	must(t, err)
	if posting.Replayed {
		t.Error("a first debit is not a replay")
	}
	posting, err = l.Debit(ctx, account, tx, 400)
	must(t, err)
	if !posting.Replayed {
		t.Error("a repeated debit should say it was replayed")
	}
	if got := balance(t, l, account); got != 600 {
		t.Errorf("balance = %d, want 600 (debited once)", got)
	}

	// A replay is answered even when the balance has since dropped below the amount.
	_, err = l.Debit(ctx, account, id(t), 600)
	must(t, err)
	if _, err := l.Debit(ctx, account, tx, 400); err != nil {
		t.Errorf("replaying an old debit once the balance is gone: %v", err)
	}
}

func reusedTransaction(t *testing.T, l application.Ledger) {
	account, tx := id(t), id(t)
	_, err := l.Credit(ctx, account, tx, 100)
	must(t, err)

	if _, err := l.Credit(ctx, account, tx, 101); !errors.Is(err, domain.ErrRejected) {
		t.Errorf("same transaction, different amount: %v, want ErrRejected", err)
	}
	if got := balance(t, l, account); got != 100 {
		t.Errorf("balance = %d, want 100", got)
	}
}

func refund(t *testing.T, l application.Ledger) {
	account, tx := id(t), id(t)
	_, err := l.Credit(ctx, account, id(t), 500)
	must(t, err)

	_, err = l.Debit(ctx, account, tx, 300) // the operation's debit
	must(t, err)
	if _, err := l.Credit(ctx, account, tx, 300); err != nil { // its refund, with the same transaction id
		t.Fatalf("a refund shares the debit's transaction id and must be accepted: %v", err)
	}
	if got := balance(t, l, account); got != 500 {
		t.Errorf("balance = %d, want 500 after debit and refund", got)
	}
	if posting, err := l.Credit(ctx, account, tx, 300); err != nil || !posting.Replayed {
		t.Errorf("a repeated refund: %+v, %v; want a replay", posting, err)
	}
}

func amounts(t *testing.T, l application.Ledger) {
	account := id(t)

	var want int64
	for _, cents := range []int64{1, 7, 10, 99, 115, 820, 1999, 123456789, 999999999999} {
		_, err := l.Credit(ctx, account, id(t), cents)
		must(t, err)
		want += cents
	}

	if got := balance(t, l, account); got != want {
		t.Errorf("balance = %d, want %d: an amount changed on the way through the float", got, want)
	}
}
