package grpcapi_test

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	walletv1 "github.com/JackMahfouz/e-wallet/services/wallet/gen/wallet/v1"
	grpcapi "github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/in/grpc"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var ctx = context.Background()

type fixture struct {
	*testsupport.Env
	client walletv1.WalletClient
	n      int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	env := testsupport.NewEnv(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(grpcapi.LoggingInterceptor(slog.New(slog.DiscardHandler))))
	walletv1.RegisterWalletServer(server, grpcapi.New(env.Svc, slog.New(slog.DiscardHandler)))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &fixture{Env: env, client: walletv1.NewWalletClient(conn)}
}

func (f *fixture) key() string {
	f.n++

	return testsupport.UUID(800_000 + f.n)
}

// account makes an account through the core, funded straight in the ledger.
func (f *fixture) account(t *testing.T, user string, cents int64, limit *int64) string {
	t.Helper()

	view, err := f.Svc.CreateAccount(ctx, user, "acct", limit)
	if err != nil {
		t.Fatal(err)
	}
	f.Ledger.Fund(view.ID, cents)

	return view.ID
}

func code(err error) codes.Code { return status.Code(err) }

func TestCreditAndDebitMoveMoneyAndReturnTheOperation(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, testsupport.UUID(1), 0, nil)

	credit, err := f.client.Credit(ctx, &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 125.5})
	if err != nil {
		t.Fatal(err)
	}
	if credit.Status != walletv1.OperationStatus_OPERATION_STATUS_COMPLETED || credit.Kind != walletv1.OperationKind_OPERATION_KIND_CREDIT ||
		credit.Amount != 125.5 || credit.Initiator != "service:payment" || credit.Spending || credit.CompletedAtMicros == 0 {
		t.Errorf("credit = %v", credit)
	}

	debit, err := f.client.Debit(ctx, &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 25.25})
	if err != nil {
		t.Fatal(err)
	}
	if debit.Status != walletv1.OperationStatus_OPERATION_STATUS_COMPLETED || !debit.Spending {
		t.Errorf("debit = %v", debit)
	}
	if got := f.Ledger.BalanceOf(account); got != 10025 {
		t.Errorf("balance = %d, want 10025", got)
	}

	got, err := f.client.GetOperation(ctx, &walletv1.GetOperationRequest{OperationId: debit.Id})
	if err != nil || got.Id != debit.Id || got.Status != walletv1.OperationStatus_OPERATION_STATUS_COMPLETED {
		t.Errorf("GetOperation = %v, %v", got, err)
	}
}

func TestTransferBetweenAccountsOfDifferentUsers(t *testing.T) {
	f := newFixture(t)
	from := f.account(t, testsupport.UUID(1), 5000, nil)
	to := f.account(t, testsupport.UUID(2), 0, nil)

	op, err := f.client.Transfer(ctx, &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: from, CounterpartyAccountId: to, Amount: 20})
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != walletv1.OperationStatus_OPERATION_STATUS_COMPLETED || op.CounterpartyAccountId != to || !op.Spending {
		t.Errorf("transfer = %v", op)
	}
	if f.Ledger.BalanceOf(from) != 3000 || f.Ledger.BalanceOf(to) != 2000 {
		t.Errorf("balances = %d, %d", f.Ledger.BalanceOf(from), f.Ledger.BalanceOf(to))
	}
}

func TestARetryWithTheSameKeyReturnsTheSameOperation(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, testsupport.UUID(1), 1000, nil)
	req := &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 3}

	first, err := f.client.Debit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.client.Debit(ctx, req)
	if err != nil || again.Id != first.Id {
		t.Fatalf("retry = %v, %v; want operation %s", again, err, first.Id)
	}
	if got := f.Ledger.BalanceOf(account); got != 700 {
		t.Errorf("balance = %d, want 700 (debited once)", got)
	}

	req.Amount = 4
	if _, err := f.client.Debit(ctx, req); code(err) != codes.AlreadyExists {
		t.Errorf("same key, other amount: %v, want ALREADY_EXISTS", err)
	}
}

func TestErrorsUseTheMatchingStatusCodes(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, testsupport.UUID(1), 1000, nil)
	other := f.account(t, testsupport.UUID(2), 0, nil)
	ok := func() *walletv1.MoveRequest {
		return &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 1}
	}

	tests := map[string]struct {
		call func() error
		want codes.Code
		text string
	}{
		"amount with three decimals": {func() error { r := ok(); r.Amount = 1.234; _, err := f.client.Debit(ctx, r); return err }, codes.InvalidArgument, "decimal"},
		"zero amount":                {func() error { r := ok(); r.Amount = 0; _, err := f.client.Credit(ctx, r); return err }, codes.InvalidArgument, "greater than 0"},
		"missing key":                {func() error { r := ok(); r.IdempotencyKey = ""; _, err := f.client.Debit(ctx, r); return err }, codes.InvalidArgument, "Idempotency"},
		"bad service":                {func() error { r := ok(); r.Service = "Pay Ment"; _, err := f.client.Debit(ctx, r); return err }, codes.InvalidArgument, "service"},
		"bad account id":             {func() error { r := ok(); r.AccountId = "42"; _, err := f.client.Debit(ctx, r); return err }, codes.InvalidArgument, "UUID"},
		"unknown account": {func() error {
			r := ok()
			r.AccountId = testsupport.UUID(999)
			_, err := f.client.Debit(ctx, r)
			return err
		}, codes.NotFound, ""},
		"overdraft":               {func() error { r := ok(); r.Amount = 11; _, err := f.client.Debit(ctx, r); return err }, codes.FailedPrecondition, "10.00 available, 11.00 requested"},
		"transfer without a peer": {func() error { _, err := f.client.Transfer(ctx, ok()); return err }, codes.InvalidArgument, ""},
		"credit with a peer": {func() error {
			r := ok()
			r.CounterpartyAccountId = other
			_, err := f.client.Credit(ctx, r)
			return err
		}, codes.InvalidArgument, "counterparty"},
		"unknown operation": {func() error {
			_, err := f.client.GetOperation(ctx, &walletv1.GetOperationRequest{OperationId: testsupport.UUID(999)})
			return err
		}, codes.NotFound, ""},
		"unknown hold": {func() error {
			_, err := f.client.ReleaseHold(ctx, &walletv1.ReleaseHoldRequest{Service: "payment", HoldId: testsupport.UUID(999)})
			return err
		}, codes.NotFound, ""},
		"unknown account to read": {func() error {
			_, err := f.client.GetAccount(ctx, &walletv1.GetAccountRequest{AccountId: testsupport.UUID(999)})
			return err
		}, codes.NotFound, ""},
	}

	for name, tc := range tests {
		err := tc.call()
		if code(err) != tc.want || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%s: err = %v, want %s containing %q", name, err, tc.want, tc.text)
		}
	}
}

func TestAnUnreachableLedgerIsUnavailable(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, testsupport.UUID(1), 1000, nil)

	f.Ledger.Fault = func(testsupport.LedgerCall) testsupport.Fault { return testsupport.FaultUnavailable }
	_, err := f.client.Debit(ctx, &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 1})

	if code(err) != codes.Unavailable {
		t.Errorf("err = %v, want UNAVAILABLE", err)
	}
}

func TestAnOperationStuckOnTheLedgerIsReturnedPendingNotAsAnError(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, testsupport.UUID(1), 1000, nil)

	f.Ledger.Fault = func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == "Debit" {
			return testsupport.FaultUnavailable
		}

		return testsupport.FaultNone
	}
	op, err := f.client.Debit(ctx, &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 1})
	if err != nil || op.Status != walletv1.OperationStatus_OPERATION_STATUS_PENDING {
		t.Fatalf("op = %v, err = %v; want a pending operation", op, err)
	}

	f.Ledger.Fault = nil
	f.Clock.Advance(time.Minute)
	if _, err := f.Svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := f.client.GetOperation(ctx, &walletv1.GetOperationRequest{OperationId: op.Id})
	if err != nil || got.Status != walletv1.OperationStatus_OPERATION_STATUS_COMPLETED {
		t.Errorf("after recovery = %v, %v", got, err)
	}
}

func TestAnOverLimitSpendComesBackAwaitingConfirmation(t *testing.T) {
	f := newFixture(t)
	limit := int64(10_000) // 100.00
	account := f.account(t, testsupport.UUID(1), 100_000, &limit)

	op, err := f.client.Debit(ctx, &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 150})
	if err != nil {
		t.Fatalf("an over-limit spend is not an error: %v", err)
	}

	if op.Status != walletv1.OperationStatus_OPERATION_STATUS_AWAITING_CONFIRMATION || op.Confirmation == nil ||
		op.Confirmation.LimitScope != walletv1.LimitScope_LIMIT_SCOPE_ACCOUNT || op.Confirmation.ExpiresAtMicros == 0 {
		t.Errorf("op = %v", op)
	}
	if got := f.Ledger.BalanceOf(account); got != 100_000 {
		t.Errorf("balance = %d: nothing may move before the owner confirms", got)
	}

	// The owner confirms through the user API, and the service sees the result.
	owner := testsupport.UUID(1)
	if _, err := f.Svc.Confirm(ctx, application.ConfirmRequest{UserID: owner, OperationID: op.Id, OTP: testsupport.ValidOTP, Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.client.GetOperation(ctx, &walletv1.GetOperationRequest{OperationId: op.Id}); got.Status != walletv1.OperationStatus_OPERATION_STATUS_COMPLETED {
		t.Errorf("after confirming = %v", got)
	}
}

func TestHoldsCanBeCreatedCapturedAndReleased(t *testing.T) {
	f := newFixture(t)
	account := f.account(t, testsupport.UUID(1), 100_000, nil)

	hold, err := f.client.CreateHold(ctx, &walletv1.CreateHoldRequest{
		Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 400, Reason: "order 9", TtlSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hold.Amount != 400 || hold.Reason != "order 9" || hold.ExpiresAtMicros == 0 || hold.ReleasedAtMicros != 0 || hold.CapturedByOperationId != "" {
		t.Errorf("hold = %v", hold)
	}

	view, err := f.client.GetAccount(ctx, &walletv1.GetAccountRequest{AccountId: account})
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance != 1000 || view.Held != 400 || view.Available != 600 || view.HasDailyLimit || view.Status != walletv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		t.Errorf("account = %v", view)
	}

	op, err := f.client.Debit(ctx, &walletv1.MoveRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 300, HoldId: hold.Id})
	if err != nil || op.Status != walletv1.OperationStatus_OPERATION_STATUS_COMPLETED {
		t.Fatalf("capture = %v, %v", op, err)
	}
	if _, err := f.client.ReleaseHold(ctx, &walletv1.ReleaseHoldRequest{Service: "payment", HoldId: hold.Id}); code(err) != codes.FailedPrecondition {
		t.Errorf("releasing a captured hold: %v, want FAILED_PRECONDITION", err)
	}

	// A second hold, released.
	second, err := f.client.CreateHold(ctx, &walletv1.CreateHoldRequest{Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 100, TtlSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	released, err := f.client.ReleaseHold(ctx, &walletv1.ReleaseHoldRequest{Service: "payment", HoldId: second.Id})
	if err != nil || released.ReleasedAtMicros == 0 {
		t.Errorf("released = %v, %v", released, err)
	}

	for name, req := range map[string]*walletv1.CreateHoldRequest{
		"no ttl":     {Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 1},
		"huge ttl":   {Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 1, TtlSeconds: 365 * 24 * 3600},
		"bad amount": {Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 0.001, TtlSeconds: 60},
		"too much":   {Service: "payment", IdempotencyKey: f.key(), AccountId: account, Amount: 999999, TtlSeconds: 60},
	} {
		_, err := f.client.CreateHold(ctx, req)
		want := codes.InvalidArgument
		if name == "too much" {
			want = codes.FailedPrecondition
		}
		if code(err) != want {
			t.Errorf("%s: err = %v, want %s", name, err, want)
		}
	}
}

func TestAccountWithALimitReportsIt(t *testing.T) {
	f := newFixture(t)
	limit := int64(2500)
	account := f.account(t, testsupport.UUID(1), 0, &limit)

	view, err := f.client.GetAccount(ctx, &walletv1.GetAccountRequest{AccountId: account})
	if err != nil || !view.HasDailyLimit || view.DailyLimit != 25 {
		t.Errorf("account = %v, %v", view, err)
	}
}
