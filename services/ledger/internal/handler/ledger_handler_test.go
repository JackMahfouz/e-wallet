package handler_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/handler"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/service"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/store"
	"github.com/JackMahfouz/e-wallet/services/ledger/internal/testdb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var ctx = context.Background()

// fakeService records what the handler passes down and answers with scripted results.
type fakeService struct {
	mu sync.Mutex

	posted      []service.PostInput
	postResult  service.PostResult
	err         error
	balance     domain.AccountBalance
	entries     []domain.Entry
	hasMore     bool
	checkpoints []domain.Checkpoint

	entriesArgs struct {
		afterSeq int64
		limit    int
	}
	checkpointsArgs struct {
		period domain.Period
		limit  int
	}
}

func (f *fakeService) Debit(_ context.Context, in service.PostInput) (service.PostResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, in)

	return f.postResult, f.err
}

func (f *fakeService) Credit(ctx context.Context, in service.PostInput) (service.PostResult, error) {
	return f.Debit(ctx, in)
}

func (f *fakeService) Balance(context.Context, string) (domain.AccountBalance, error) {
	return f.balance, f.err
}

func (f *fakeService) ListEntries(_ context.Context, _ string, afterSeq int64, limit int) ([]domain.Entry, bool, error) {
	f.entriesArgs.afterSeq, f.entriesArgs.limit = afterSeq, limit

	return f.entries, f.hasMore, f.err
}

func (f *fakeService) ListCheckpoints(_ context.Context, _ string, period domain.Period, limit int) ([]domain.Checkpoint, error) {
	f.checkpointsArgs.period, f.checkpointsArgs.limit = period, limit

	return f.checkpoints, f.err
}

func newHandler(svc handler.LedgerService) (*handler.Handler, *bytes.Buffer) {
	var logs bytes.Buffer

	return handler.New(svc, slog.New(slog.NewTextHandler(&logs, nil))), &logs
}

func TestPostMultipliesByOneHundredAndPassesCentsDown(t *testing.T) {
	account, transaction := testdb.UUID(), testdb.UUID()
	created := time.Date(2026, 11, 10, 9, 8, 7, 654321000, time.UTC)

	for name, call := range map[string]func(*handler.Handler, *ledgerv1.PostRequest) (*ledgerv1.PostResponse, error){
		"debit": func(h *handler.Handler, r *ledgerv1.PostRequest) (*ledgerv1.PostResponse, error) {
			return h.Debit(ctx, r)
		},
		"credit": func(h *handler.Handler, r *ledgerv1.PostRequest) (*ledgerv1.PostResponse, error) {
			return h.Credit(ctx, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeService{postResult: service.PostResult{
				Entry:    domain.Entry{ID: "entry-1", AccountID: account, TransactionID: transaction, Seq: 7, Direction: domain.Credit, Amount: 1999, CreatedAt: created},
				Balance:  123456,
				Replayed: true,
			}}
			h, _ := newHandler(svc)

			response, err := call(h, &ledgerv1.PostRequest{AccountId: account, TransactionId: transaction, Amount: 19.99})

			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(svc.posted) != 1 || svc.posted[0] != (service.PostInput{AccountID: account, TransactionID: transaction, Amount: 1999}) {
				t.Errorf("the service received %+v, want 19.99 as 1999 cents", svc.posted)
			}
			entry := response.GetEntry()
			if response.GetBalance() != 1234.56 || !response.GetReplayed() {
				t.Errorf("balance = %v, replayed = %v; want 1234.56 (123456 cents / 100) and true", response.GetBalance(), response.GetReplayed())
			}
			if entry.GetAmount() != 19.99 || entry.GetSeq() != 7 || entry.GetDirection() != ledgerv1.Direction_DIRECTION_CREDIT ||
				entry.GetId() != "entry-1" || entry.GetCreatedAtMicros() != created.UnixMicro() {
				t.Errorf("entry = %+v", entry)
			}
		})
	}
}

func TestPostRefusesAmountsThatAreNotWholeCentsWithoutCallingTheService(t *testing.T) {
	amounts := map[string]float64{
		"zero":                 0,
		"negative":             -1,
		"not a number":         math.NaN(),
		"infinity":             math.Inf(1),
		"half a cent too many": 10.005,
		"a tenth of a cent":    0.001,
		"too large":            1e12,
	}

	for name, amount := range amounts {
		t.Run(name, func(t *testing.T) {
			svc := &fakeService{}
			h, _ := newHandler(svc)
			request := &ledgerv1.PostRequest{AccountId: testdb.UUID(), TransactionId: testdb.UUID(), Amount: amount}

			_, debitErr := h.Debit(ctx, request)
			_, creditErr := h.Credit(ctx, request)

			for _, err := range []error{debitErr, creditErr} {
				if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "invalid amount") {
					t.Errorf("err = %v, want InvalidArgument naming the amount", err)
				}
			}
			if len(svc.posted) != 0 {
				t.Errorf("the service was called %d times for an invalid amount", len(svc.posted))
			}
		})
	}
}

func TestErrorsBecomeTheRightStatusCodes(t *testing.T) {
	tests := map[string]struct {
		err     error
		code    codes.Code
		message string
	}{
		"invalid id":           {fmt.Errorf("%w: account_id must be a UUID", domain.ErrInvalidID), codes.InvalidArgument, "account_id must be a UUID"},
		"invalid amount":       {fmt.Errorf("%w: must be greater than 0", domain.ErrInvalidAmount), codes.InvalidArgument, "must be greater than 0"},
		"invalid period":       {fmt.Errorf("%w: nope", domain.ErrInvalidPeriod), codes.InvalidArgument, "nope"},
		"insufficient funds":   {&domain.InsufficientFundsError{Balance: 500, Requested: 1050}, codes.FailedPrecondition, "insufficient funds: balance 5.00, requested 10.50"},
		"transaction conflict": {domain.ErrTransactionConflict, codes.AlreadyExists, "different amount"},
		"deadline exceeded":    {fmt.Errorf("query: %w", context.DeadlineExceeded), codes.DeadlineExceeded, "deadline exceeded"},
		"cancelled":            {fmt.Errorf("query: %w", context.Canceled), codes.Canceled, "cancelled"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, _ := newHandler(&fakeService{err: tc.err})

			_, err := h.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: testdb.UUID()})

			if status.Code(err) != tc.code || !strings.Contains(status.Convert(err).Message(), tc.message) {
				t.Errorf("err = %v, want %v containing %q", err, tc.code, tc.message)
			}
		})
	}
}

func TestUnexpectedErrorsAreLoggedButNeverShownToTheCaller(t *testing.T) {
	secret := errors.New(`pq: password authentication failed for user "ledger" at 10.0.0.5`)
	h, logs := newHandler(&fakeService{err: secret})

	_, err := h.Debit(ctx, &ledgerv1.PostRequest{AccountId: testdb.UUID(), TransactionId: testdb.UUID(), Amount: 1})

	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
		t.Errorf("err = %v, want a generic Internal error", err)
	}
	if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "10.0.0.5") {
		t.Errorf("the caller saw internal details: %v", err)
	}
	if !strings.Contains(logs.String(), "password authentication failed") {
		t.Errorf("the real error was not logged:\n%s", logs)
	}
}

func TestGetBalanceDividesByOneHundred(t *testing.T) {
	h, _ := newHandler(&fakeService{balance: domain.AccountBalance{Balance: 7805, LastSeq: 12}})

	response, err := h.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: testdb.UUID()})

	if err != nil || response.GetBalance() != 78.05 || response.GetLastSeq() != 12 {
		t.Errorf("response = %+v, %v; want 78.05 at seq 12", response, err)
	}
}

func TestListEntriesConvertsAmountsAndPassesThePaging(t *testing.T) {
	svc := &fakeService{
		hasMore: true,
		entries: []domain.Entry{
			{ID: "a", Seq: 4, Direction: domain.Debit, Amount: 5, CreatedAt: time.Unix(10, 0)},
			{ID: "b", Seq: 5, Direction: domain.Credit, Amount: 100000, CreatedAt: time.Unix(20, 0)},
		},
	}
	h, _ := newHandler(svc)

	response, err := h.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: testdb.UUID(), AfterSeq: 3, Limit: 2})

	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if svc.entriesArgs.afterSeq != 3 || svc.entriesArgs.limit != 2 {
		t.Errorf("paging passed down = %+v, want after 3, limit 2", svc.entriesArgs)
	}
	entries := response.GetEntries()
	if !response.GetHasMore() || len(entries) != 2 ||
		entries[0].GetAmount() != 0.05 || entries[0].GetDirection() != ledgerv1.Direction_DIRECTION_DEBIT ||
		entries[1].GetAmount() != 1000 || entries[1].GetDirection() != ledgerv1.Direction_DIRECTION_CREDIT {
		t.Errorf("response = %+v", response)
	}
}

func TestListCheckpointsMapsPeriodsAndConvertsBalances(t *testing.T) {
	start, end := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	svc := &fakeService{checkpoints: []domain.Checkpoint{
		{ID: "c1", Period: domain.PeriodFiveYear, PeriodStart: start, PeriodEnd: end, LastSeq: 9, Balance: 123456, CreatedAt: end},
	}}
	h, _ := newHandler(svc)

	response, err := h.ListCheckpoints(ctx, &ledgerv1.ListCheckpointsRequest{AccountId: testdb.UUID(), Period: ledgerv1.Period_PERIOD_QUARTER, Limit: 5})

	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if svc.checkpointsArgs.period != domain.PeriodQuarter || svc.checkpointsArgs.limit != 5 {
		t.Errorf("filter passed down = %+v, want quarter, limit 5", svc.checkpointsArgs)
	}
	c := response.GetCheckpoints()[0]
	if c.GetPeriod() != ledgerv1.Period_PERIOD_FIVE_YEAR || c.GetBalance() != 1234.56 || c.GetLastSeq() != 9 ||
		c.GetPeriodStartMicros() != start.UnixMicro() || c.GetPeriodEndMicros() != end.UnixMicro() {
		t.Errorf("checkpoint = %+v", c)
	}
}

func TestListCheckpointsTreatsAnUnspecifiedPeriodAsAll(t *testing.T) {
	svc := &fakeService{}
	h, _ := newHandler(svc)

	if _, err := h.ListCheckpoints(ctx, &ledgerv1.ListCheckpointsRequest{AccountId: testdb.UUID()}); err != nil {
		t.Fatalf("err = %v", err)
	}

	if svc.checkpointsArgs.period != "" {
		t.Errorf("period passed down = %q, want empty (all periods)", svc.checkpointsArgs.period)
	}
}

func TestListCheckpointsRefusesAnUnknownPeriod(t *testing.T) {
	svc := &fakeService{}
	h, _ := newHandler(svc)

	_, err := h.ListCheckpoints(ctx, &ledgerv1.ListCheckpointsRequest{AccountId: testdb.UUID(), Period: ledgerv1.Period(99)})

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestTheAPIOffersNoWayToChangeOrRemoveAnEntry(t *testing.T) {
	var methods []string
	for _, method := range ledgerv1.Ledger_ServiceDesc.Methods {
		methods = append(methods, method.MethodName)
	}
	sort.Strings(methods)

	want := []string{"Credit", "Debit", "GetBalance", "ListCheckpoints", "ListEntries"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Errorf("methods = %v, want exactly %v: the ledger is append-only, so there is no update or delete", methods, want)
	}
	if len(ledgerv1.Ledger_ServiceDesc.Streams) != 0 {
		t.Errorf("the ledger unexpectedly exposes %d streaming RPCs", len(ledgerv1.Ledger_ServiceDesc.Streams))
	}
}

// startServer runs the real handler, service and PostgreSQL store behind a loopback gRPC server.
func startServer(t *testing.T) ledgerv1.LedgerClient {
	t.Helper()

	pool := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(handler.LoggingInterceptor(logger)))
	ledgerv1.RegisterLedgerServer(server, handler.New(service.New(store.New(pool)), logger))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return ledgerv1.NewLedgerClient(conn)
}

func post(t *testing.T, call func(context.Context, *ledgerv1.PostRequest, ...grpc.CallOption) (*ledgerv1.PostResponse, error), account string, amount float64) *ledgerv1.PostResponse {
	t.Helper()

	response, err := call(ctx, &ledgerv1.PostRequest{AccountId: account, TransactionId: testdb.UUID(), Amount: amount})
	if err != nil {
		t.Fatalf("post %v: %v", amount, err)
	}

	return response
}

func TestAmountsAreFloatsOnTheWireAndWholeCentsUnderneath(t *testing.T) {
	client := startServer(t)
	account := testdb.UUID()

	credited := post(t, client.Credit, account, 100.50)
	debited := post(t, client.Debit, account, 0.1+0.2) // 0.30000000000000004: float noise, 30 cents
	balance, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: account})

	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if credited.GetBalance() != 100.5 || debited.GetBalance() != 100.2 || balance.GetBalance() != 100.2 || balance.GetLastSeq() != 2 {
		t.Errorf("balances = %v, %v, %v at seq %d; want 100.5, 100.2, 100.2 at seq 2", credited.GetBalance(), debited.GetBalance(), balance.GetBalance(), balance.GetLastSeq())
	}
	entries, err := client.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: account})
	if err != nil || len(entries.GetEntries()) != 2 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	if first, second := entries.GetEntries()[0], entries.GetEntries()[1]; first.GetAmount() != 100.5 || second.GetAmount() != 0.3 {
		t.Errorf("entry amounts = %v and %v, want 100.5 and 0.3", first.GetAmount(), second.GetAmount())
	}
	if age := time.Since(time.UnixMicro(credited.GetEntry().GetCreatedAtMicros())); age < 0 || age > time.Minute {
		t.Errorf("created_at is %v ago, want just now", age)
	}
}

func TestOverdraftsAreRefusedOverGRPC(t *testing.T) {
	client := startServer(t)
	account := testdb.UUID()
	post(t, client.Credit, account, 10)

	_, err := client.Debit(ctx, &ledgerv1.PostRequest{AccountId: account, TransactionId: testdb.UUID(), Amount: 10.01})

	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "balance 10.00, requested 10.01") {
		t.Errorf("err = %v, want FailedPrecondition naming both amounts", err)
	}
	_, err = client.Debit(ctx, &ledgerv1.PostRequest{AccountId: testdb.UUID(), TransactionId: testdb.UUID(), Amount: 1})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a debit on an empty account: err = %v, want FailedPrecondition", err)
	}
}

func TestRetriesAndConflictsOverGRPC(t *testing.T) {
	client := startServer(t)
	account, transaction := testdb.UUID(), testdb.UUID()
	request := &ledgerv1.PostRequest{AccountId: account, TransactionId: transaction, Amount: 25}

	first, err := client.Credit(ctx, request)
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
	retry, err := client.Credit(ctx, request)

	if err != nil || first.GetReplayed() || !retry.GetReplayed() || retry.GetEntry().GetId() != first.GetEntry().GetId() {
		t.Errorf("first = %+v, retry = %+v, err = %v; want the retry to replay the same entry", first, retry, err)
	}
	if retry.GetBalance() != 25 {
		t.Errorf("balance after a retry = %v, want 25 (credited once)", retry.GetBalance())
	}

	_, err = client.Credit(ctx, &ledgerv1.PostRequest{AccountId: account, TransactionId: transaction, Amount: 26})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("reusing the transaction id with another amount: err = %v, want AlreadyExists", err)
	}
}

func TestBadInputOverGRPC(t *testing.T) {
	client := startServer(t)

	_, creditErr := client.Credit(ctx, &ledgerv1.PostRequest{AccountId: "alice", TransactionId: testdb.UUID(), Amount: 5})
	_, balanceErr := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: ""})
	_, entriesErr := client.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: "x"})
	_, amountErr := client.Credit(ctx, &ledgerv1.PostRequest{AccountId: testdb.UUID(), TransactionId: testdb.UUID(), Amount: 5.555})

	for name, err := range map[string]error{"credit": creditErr, "balance": balanceErr, "entries": entriesErr, "amount": amountErr} {
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", name, err)
		}
	}
}

func TestAnAccountWithNoEntriesReadsAsEmpty(t *testing.T) {
	client := startServer(t)
	account := testdb.UUID()

	balance, balanceErr := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: account})
	entries, entriesErr := client.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: account})
	checkpoints, checkpointsErr := client.ListCheckpoints(ctx, &ledgerv1.ListCheckpointsRequest{AccountId: account})

	if balanceErr != nil || entriesErr != nil || checkpointsErr != nil {
		t.Fatalf("errors: %v, %v, %v", balanceErr, entriesErr, checkpointsErr)
	}
	if balance.GetBalance() != 0 || balance.GetLastSeq() != 0 || len(entries.GetEntries()) != 0 || entries.GetHasMore() || len(checkpoints.GetCheckpoints()) != 0 {
		t.Errorf("balance = %+v, entries = %+v, checkpoints = %+v; want all empty", balance, entries, checkpoints)
	}
}

func TestEntriesPageOverGRPC(t *testing.T) {
	client := startServer(t)
	account := testdb.UUID()
	for i := 1; i <= 5; i++ {
		post(t, client.Credit, account, float64(i))
	}

	first, err := client.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: account, Limit: 2})
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	last := first.GetEntries()[len(first.GetEntries())-1].GetSeq()
	second, err := client.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: account, AfterSeq: last, Limit: 10})

	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(first.GetEntries()) != 2 || !first.GetHasMore() || len(second.GetEntries()) != 3 || second.GetHasMore() || second.GetEntries()[0].GetSeq() != 3 {
		t.Errorf("first = %+v, second = %+v", first, second)
	}
}

func TestConcurrentDebitsOverGRPCNeverOverdraw(t *testing.T) {
	client := startServer(t)
	account := testdb.UUID()
	post(t, client.Credit, account, 10)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
		refused int
	)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.Debit(ctx, &ledgerv1.PostRequest{AccountId: account, TransactionId: testdb.UUID(), Amount: 1})

			mu.Lock()
			defer mu.Unlock()
			switch status.Code(err) {
			case codes.OK:
				granted++
			case codes.FailedPrecondition:
				refused++
			default:
				t.Errorf("unexpected result: %v", err)
			}
		}()
	}
	wg.Wait()

	balance, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: account})
	if err != nil || granted != 10 || refused != 30 || balance.GetBalance() != 0 {
		t.Errorf("granted %d, refused %d, balance %v, err %v; want 10, 30 and 0", granted, refused, balance.GetBalance(), err)
	}
}
