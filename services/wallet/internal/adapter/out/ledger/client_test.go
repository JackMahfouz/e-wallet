package ledger_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	ledgeradapter "github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/out/ledger"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport/ledgercontract"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var ctx = context.Background()

func newClient(t *testing.T) (*ledgeradapter.Client, *testsupport.Ledger, *testsupport.LedgerServer) {
	t.Helper()

	fake := &testsupport.Ledger{}
	server := &testsupport.LedgerServer{Ledger: fake}

	return ledgeradapter.New(testsupport.StartLedgerServer(t, server), 2*time.Second), fake, server
}

func TestClientMeetsTheLedgerContractOverGRPC(t *testing.T) {
	ledgercontract.Run(t, func(t *testing.T) application.Ledger {
		client, _, _ := newClient(t)

		return client
	})
}

// TestClientMeetsTheLedgerContractAgainstTheRealLedger runs the same suite against a running ledger
// service. It needs one: WALLET_TEST_LEDGER_ADDR=127.0.0.1:9091 (the full-chain run does this).
func TestClientMeetsTheLedgerContractAgainstTheRealLedger(t *testing.T) {
	addr := os.Getenv("WALLET_TEST_LEDGER_ADDR")
	if addr == "" {
		t.Skip("WALLET_TEST_LEDGER_ADDR is not set: skipping the test against the real ledger")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ledgercontract.Run(t, func(t *testing.T) application.Ledger {
		return ledgeradapter.New(ledgerv1.NewLedgerClient(conn), 5*time.Second)
	})

	// Things only the real ledger checks.
	client := ledgeradapter.New(ledgerv1.NewLedgerClient(conn), 5*time.Second)
	if _, err := client.Credit(ctx, "not-a-uuid", testsupport.NewRandomUUID(), 100); !errors.Is(err, domain.ErrRejected) {
		t.Errorf("a bad account id: %v, want ErrRejected", err)
	}
	if _, err := client.Credit(ctx, testsupport.NewRandomUUID(), testsupport.NewRandomUUID(), 0); !errors.Is(err, domain.ErrRejected) {
		t.Errorf("a zero amount: %v, want ErrRejected", err)
	}
}

func TestInsufficientFundsCarriesWhatWasAvailable(t *testing.T) {
	client, fake, _ := newClient(t)
	account := testsupport.NewRandomUUID()
	fake.Fund(account, 1250)

	var insufficient *domain.InsufficientFundsError
	_, err := client.Debit(ctx, account, testsupport.NewRandomUUID(), 2000)

	if !errors.As(err, &insufficient) || insufficient.Available != 1250 || insufficient.Requested != 2000 {
		t.Errorf("err = %v, want insufficient funds with 1250 available and 2000 requested", err)
	}
}

func TestAnUnreachableOrFailingLedgerIsUnavailableNotRejected(t *testing.T) {
	client, fake, _ := newClient(t)
	account := testsupport.NewRandomUUID()

	fake.Fault = func(testsupport.LedgerCall) testsupport.Fault { return testsupport.FaultUnavailable }
	for name, call := range map[string]func() error{
		"balance": func() error { _, err := client.Balance(ctx, account); return err },
		"debit":   func() error { _, err := client.Debit(ctx, account, testsupport.NewRandomUUID(), 1); return err },
		"credit":  func() error { _, err := client.Credit(ctx, account, testsupport.NewRandomUUID(), 1); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrRejected) {
			t.Errorf("%s: err = %v, want ErrUnavailable", name, err)
		}
	}

	fake.Fault = func(testsupport.LedgerCall) testsupport.Fault { return testsupport.FaultRejected }
	if _, err := client.Credit(ctx, account, testsupport.NewRandomUUID(), 1); !errors.Is(err, domain.ErrRejected) {
		t.Errorf("a refusal: %v, want ErrRejected", err)
	}
}

func TestASlowLedgerTimesOutAsUnavailable(t *testing.T) {
	fake := &testsupport.Ledger{}
	server := &testsupport.LedgerServer{Ledger: fake, Delay: time.Second}
	client := ledgeradapter.New(testsupport.StartLedgerServer(t, server), 100*time.Millisecond)

	start := time.Now()
	_, err := client.Credit(ctx, testsupport.NewRandomUUID(), testsupport.NewRandomUUID(), 5)

	if !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Errorf("the call took %s: the timeout was not applied", elapsed)
	}
}

func TestADeadLedgerIsUnavailable(t *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := ledgeradapter.New(ledgerv1.NewLedgerClient(conn), 500*time.Millisecond)

	_, err = client.Balance(ctx, testsupport.NewRandomUUID())
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if err != nil && !strings.Contains(err.Error(), "ledger balance") {
		t.Errorf("err = %v, should say what was being done", err)
	}
}

func TestAnAmountTheLedgerCannotRepresentIsRejectedBeforeItIsSent(t *testing.T) {
	client, _, _ := newClient(t)

	// 1e10 + 1 cents is over the ledger's maximum posting: the ledger refuses, and so it is rejected.
	_, err := client.Credit(ctx, testsupport.NewRandomUUID(), testsupport.NewRandomUUID(), 1_000_000_000_001)
	if !errors.Is(err, domain.ErrRejected) {
		t.Errorf("err = %v, want ErrRejected", err)
	}
}
