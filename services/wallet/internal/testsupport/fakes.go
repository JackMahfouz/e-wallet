// Package testsupport holds in-memory stand-ins for the wallet's ports, so the core can be tested
// without a database or a network, and so adapter tests can share a wired-up Service.
//
// The fakes are not shortcuts around the real thing: the store fake and the PostgreSQL adapter run
// the same contract tests, and the ledger fake follows the real ledger's rules (idempotent per
// account, transaction and direction; no overdraft).
package testsupport

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// Clock is a clock that only moves when told to. Times are whole microseconds, like the
// database's.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts at a fixed Wednesday noon, UTC.
func NewClock() *Clock {
	return &Clock{now: time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// Advance moves the clock on.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d).Truncate(time.Microsecond)
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC().Truncate(time.Microsecond)
}

// SeqIDs hands out predictable UUIDs.
type SeqIDs struct {
	mu sync.Mutex
	n  int
}

func (g *SeqIDs) NewID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++

	return fmt.Sprintf("00000000-0000-4000-8000-%012d", g.n)
}

// UUID returns a fixed UUID built from n, for tests that need ids of their own.
func UUID(n int) string { return fmt.Sprintf("11111111-1111-4111-8111-%012d", n) }

// Tokens verifies bearer tokens from a table.
type Tokens struct {
	mu    sync.Mutex
	users map[string]string
	Down  bool
}

// Add makes token valid for user.
func (t *Tokens) Add(token, userID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.users == nil {
		t.users = map[string]string{}
	}
	t.users[token] = userID
}

func (t *Tokens) Verify(_ context.Context, token string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.Down {
		return "", fmt.Errorf("%w: auth service", domain.ErrUnavailable)
	}
	user, ok := t.users[token]
	if !ok {
		return "", domain.ErrUnauthenticated
	}

	return user, nil
}

// ValidOTP is the one-time code the OTP fake accepts.
const ValidOTP = "123456"

// OTP accepts ValidOTP and nothing else.
type OTP struct{}

func (OTP) Verify(_ context.Context, _, code string) error {
	if code != ValidOTP {
		return domain.ErrOTPInvalid
	}

	return nil
}

// Fault says how a fake dependency misbehaves for one call.
type Fault int

const (
	FaultNone Fault = iota
	// FaultUnavailable fails the call before it has any effect.
	FaultUnavailable
	// FaultLostResponse applies the call, then reports it unavailable: the effect happened and the
	// caller does not know.
	FaultLostResponse
	// FaultRejected refuses the call for good.
	FaultRejected
)

func (f Fault) err(what string) error {
	switch f {
	case FaultUnavailable, FaultLostResponse:
		return fmt.Errorf("%w: %s", domain.ErrUnavailable, what)
	case FaultRejected:
		return fmt.Errorf("%w: %s", domain.ErrRejected, what)
	default:
		return nil
	}
}

// Funding is a funding gateway that remembers each reference it has seen.
type Funding struct {
	mu        sync.Mutex
	Collected map[string]int64
	PaidOut   map[string]int64
	// Fault, if set, is asked about every call ("collect" or "payout") and may fail it.
	Fault func(method, reference string) Fault
}

func (f *Funding) call(method, reference string, cents int64, into *map[string]int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	fault := FaultNone
	if f.Fault != nil {
		fault = f.Fault(method, reference)
	}
	if fault == FaultUnavailable || fault == FaultRejected {
		return fault.err(method)
	}

	if *into == nil {
		*into = map[string]int64{}
	}
	(*into)[reference] = cents

	return fault.err(method)
}

func (f *Funding) Collect(_ context.Context, reference string, cents int64) error {
	return f.call("collect", reference, cents, &f.Collected)
}

func (f *Funding) Payout(_ context.Context, reference string, cents int64) error {
	return f.call("payout", reference, cents, &f.PaidOut)
}

// LedgerCall describes one call to the fake ledger, for fault injection.
type LedgerCall struct {
	Method      string // "Balance", "Debit" or "Credit"
	Account     string
	Transaction string
	Cents       int64
}

type ledgerKey struct {
	account, transaction, direction string
}

// Ledger is a stateful stand-in for the real ledger, with its rules: a debit above the balance is
// refused, and each (account, transaction, direction) is applied once.
type Ledger struct {
	mu       sync.Mutex
	balances map[string]int64
	posted   map[ledgerKey]int64
	// Fault, if set, is asked about every call and may fail it.
	Fault func(LedgerCall) Fault
	// Calls counts calls by method, applied or not.
	Calls map[string]int
}

func (l *Ledger) init() {
	if l.balances == nil {
		l.balances = map[string]int64{}
		l.posted = map[ledgerKey]int64{}
		l.Calls = map[string]int{}
	}
}

// Fund puts money in an account directly, without a ledger call.
func (l *Ledger) Fund(account string, cents int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	l.balances[account] += cents
}

// Balance reports an account's balance, bypassing the fault hook (for assertions).
func (l *Ledger) BalanceOf(account string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.balances[account]
}

// Total is the sum of every balance: money only ever moves, so it only changes by funding.
func (l *Ledger) Total() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	var total int64
	for _, balance := range l.balances {
		total += balance
	}

	return total
}

// Posted reports whether (account, transaction, direction) has been applied, and for how much.
func (l *Ledger) Posted(account, transaction, direction string) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cents, ok := l.posted[ledgerKey{account, transaction, direction}]

	return cents, ok
}

// CallCount returns how many times method was called.
func (l *Ledger) CallCount(method string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.Calls[method]
}

func (l *Ledger) fault(call LedgerCall) Fault {
	l.init()
	l.Calls[call.Method]++

	if l.Fault == nil {
		return FaultNone
	}

	return l.Fault(call)
}

func (l *Ledger) Balance(_ context.Context, account string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	fault := l.fault(LedgerCall{Method: "Balance", Account: account})
	if fault != FaultNone {
		return 0, fault.err("ledger balance")
	}

	return l.balances[account], nil
}

func (l *Ledger) Debit(_ context.Context, account, transaction string, cents int64) (application.LedgerPosting, error) {
	return l.post("Debit", "debit", account, transaction, cents)
}

func (l *Ledger) Credit(_ context.Context, account, transaction string, cents int64) (application.LedgerPosting, error) {
	return l.post("Credit", "credit", account, transaction, cents)
}

func (l *Ledger) post(method, direction, account, transaction string, cents int64) (application.LedgerPosting, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	fault := l.fault(LedgerCall{Method: method, Account: account, Transaction: transaction, Cents: cents})
	if fault == FaultUnavailable || fault == FaultRejected {
		return application.LedgerPosting{}, fault.err("ledger " + direction)
	}

	key := ledgerKey{account, transaction, direction}
	if previous, ok := l.posted[key]; ok {
		if previous != cents {
			return application.LedgerPosting{}, fmt.Errorf("%w: transaction already posted with a different amount", domain.ErrRejected)
		}

		return application.LedgerPosting{Replayed: true}, fault.err("ledger " + direction)
	}

	if direction == "debit" {
		if l.balances[account] < cents {
			return application.LedgerPosting{}, &domain.InsufficientFundsError{Available: l.balances[account], Requested: cents}
		}
		l.balances[account] -= cents
	} else {
		l.balances[account] += cents
	}
	l.posted[key] = cents

	return application.LedgerPosting{}, fault.err("ledger " + direction)
}

// NewRandomUUID returns a random version-4 UUID, for tests that must not collide with ids already
// in a shared database.
func NewRandomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
