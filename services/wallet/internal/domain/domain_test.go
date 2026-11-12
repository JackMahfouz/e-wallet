package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

func at(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 30, 0, 0, time.UTC)
}

func TestEnumCodesAreExplicitAndStable(t *testing.T) {
	// These codes are stored in smallint columns: changing one is a data migration.
	tests := map[string]struct{ got, want int16 }{
		"account active":        {int16(domain.AccountActive), 1},
		"account frozen":        {int16(domain.AccountFrozen), 2},
		"account closed":        {int16(domain.AccountClosed), 3},
		"kind credit":           {int16(domain.KindCredit), 1},
		"kind debit":            {int16(domain.KindDebit), 2},
		"kind transfer":         {int16(domain.KindTransfer), 3},
		"kind deposit":          {int16(domain.KindDeposit), 4},
		"kind withdrawal":       {int16(domain.KindWithdrawal), 5},
		"status awaiting":       {int16(domain.StatusAwaitingConfirmation), 1},
		"status pending":        {int16(domain.StatusPending), 2},
		"status settling":       {int16(domain.StatusSettling), 3},
		"status completed":      {int16(domain.StatusCompleted), 4},
		"status failed":         {int16(domain.StatusFailed), 5},
		"failure insufficient":  {int16(domain.FailureInsufficientFunds), 1},
		"failure timed out":     {int16(domain.FailureTimedOut), 2},
		"failure conf. expired": {int16(domain.FailureConfirmationExpired), 3},
		"failure rejected":      {int16(domain.FailureRejected), 4},
		"failure cancelled":     {int16(domain.FailureCancelled), 5},
		"limit scope account":   {int16(domain.ScopeAccount), 1},
		"limit scope wallet":    {int16(domain.ScopeWallet), 2},
	}

	for name, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", name, tc.got, tc.want)
		}
	}
}

func TestEveryEnumValueHasANameAndUnknownOnesAreInvalid(t *testing.T) {
	for _, v := range domain.AccountStatuses {
		if !v.Valid() || strings.Contains(v.String(), "(") {
			t.Errorf("account status %d: valid=%v name=%q", v, v.Valid(), v)
		}
	}
	for _, v := range domain.OperationKinds {
		if !v.Valid() || strings.Contains(v.String(), "(") {
			t.Errorf("kind %d: valid=%v name=%q", v, v.Valid(), v)
		}
	}
	for _, v := range domain.OperationStatuses {
		if !v.Valid() || strings.Contains(v.String(), "(") {
			t.Errorf("status %d: valid=%v name=%q", v, v.Valid(), v)
		}
	}
	for _, v := range domain.FailureReasons {
		if !v.Valid() || strings.Contains(v.String(), "(") {
			t.Errorf("failure %d: valid=%v name=%q", v, v.Valid(), v)
		}
	}
	for _, v := range domain.LimitScopes {
		if !v.Valid() || strings.Contains(v.String(), "(") {
			t.Errorf("scope %d: valid=%v name=%q", v, v.Valid(), v)
		}
	}

	if domain.AccountStatus(0).Valid() || domain.AccountStatus(4).Valid() ||
		domain.OperationKind(0).Valid() || domain.OperationKind(6).Valid() ||
		domain.OperationStatus(0).Valid() || domain.OperationStatus(6).Valid() ||
		domain.FailureReason(0).Valid() || domain.FailureReason(6).Valid() ||
		domain.LimitScope(0).Valid() || domain.LimitScope(3).Valid() {
		t.Error("an out-of-range code reported itself valid")
	}
}

func TestOnlyOutgoingKindsMoveMoneyOut(t *testing.T) {
	want := map[domain.OperationKind]bool{
		domain.KindCredit: false, domain.KindDeposit: false,
		domain.KindDebit: true, domain.KindTransfer: true, domain.KindWithdrawal: true,
	}

	for kind, out := range want {
		if kind.MovesMoneyOut() != out {
			t.Errorf("%s.MovesMoneyOut() = %v, want %v", kind, kind.MovesMoneyOut(), out)
		}
	}
}

func TestStatusTerminalAndInFlight(t *testing.T) {
	want := map[domain.OperationStatus]bool{ // in flight?
		domain.StatusAwaitingConfirmation: true, domain.StatusPending: true, domain.StatusSettling: true,
		domain.StatusCompleted: false, domain.StatusFailed: false,
	}

	for status, inFlight := range want {
		if status.InFlight() != inFlight || status.Terminal() == inFlight {
			t.Errorf("%s: InFlight=%v Terminal=%v, want InFlight=%v", status, status.InFlight(), status.Terminal(), inFlight)
		}
	}
	if domain.OperationStatus(0).InFlight() {
		t.Error("an unknown status is not in flight")
	}
}

func TestWorkingDaysAfter(t *testing.T) {
	// 11 Nov 2026 is a Wednesday.
	tests := map[string]struct {
		start time.Time
		days  int
		want  time.Time
	}{
		"zero days is now":                  {at(2026, 11, 11, 9), 0, at(2026, 11, 11, 9)},
		"wednesday plus five is next wed":   {at(2026, 11, 11, 9), 5, at(2026, 11, 18, 9)},
		"friday plus one is monday":         {at(2026, 11, 13, 9), 1, at(2026, 11, 16, 9)},
		"friday plus five is next friday":   {at(2026, 11, 13, 9), 5, at(2026, 11, 20, 9)},
		"saturday plus one is monday":       {at(2026, 11, 14, 9), 1, at(2026, 11, 16, 9)},
		"sunday plus five is friday":        {at(2026, 11, 15, 9), 5, at(2026, 11, 20, 9)},
		"thursday plus two skips a weekend": {at(2026, 11, 12, 9), 2, at(2026, 11, 16, 9)},
		"month end":                         {at(2026, 10, 30, 9), 1, at(2026, 11, 2, 9)},
		"year end":                          {at(2026, 12, 31, 9), 5, at(2027, 1, 7, 9)},
		"ten working days is two weeks":     {at(2026, 11, 11, 9), 10, at(2026, 11, 25, 9)},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := domain.WorkingDaysAfter(tc.start, tc.days); !got.Equal(tc.want) {
				t.Errorf("WorkingDaysAfter(%s, %d) = %s, want %s", tc.start.Format(time.RFC1123), tc.days, got.Format(time.RFC1123), tc.want.Format(time.RFC1123))
			}
		})
	}
}

func TestWorkingDaysAfterIsComputedInUTC(t *testing.T) {
	zone := time.FixedZone("far-east", 13*3600)
	// 01:00 on Saturday at +13:00 is Friday 12:00 in UTC, so the working-day count starts on a
	// working day and one day on is Monday 12:00 UTC.
	local := time.Date(2026, 11, 14, 1, 0, 0, 0, zone)

	got := domain.WorkingDaysAfter(local, 1)
	want := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestIsWorkingDayAndStartOfDay(t *testing.T) {
	for day, want := range map[int]bool{9: true, 10: true, 11: true, 12: true, 13: true, 14: false, 15: false, 16: true} {
		if got := domain.IsWorkingDay(at(2026, 11, day, 0)); got != want {
			t.Errorf("IsWorkingDay(%d Nov) = %v, want %v", day, got, want)
		}
	}

	zone := time.FixedZone("west", -5*3600)
	got := domain.StartOfDay(time.Date(2026, 11, 11, 22, 0, 0, 0, zone)) // 03:00 on the 12th in UTC
	if want := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("StartOfDay = %s, want %s", got, want)
	}
}

func ptr(v int64) *int64 { return &v }

func TestLimitSumOK(t *testing.T) {
	tests := map[string]struct {
		wallet   *int64
		accounts []*int64
		want     bool
	}{
		"no wallet limit imposes nothing":         {nil, []*int64{ptr(1_000_000)}, true},
		"sum below the wallet limit":              {ptr(100), []*int64{ptr(40), ptr(50)}, true},
		"sum equal to the wallet limit":           {ptr(100), []*int64{ptr(40), ptr(60)}, true},
		"sum above the wallet limit":              {ptr(100), []*int64{ptr(40), ptr(61)}, false},
		"accounts without limits are not counted": {ptr(100), []*int64{ptr(100), nil, nil}, true},
		"a single account above the wallet limit": {ptr(100), []*int64{ptr(101)}, false},
		"no accounts": {ptr(100), nil, true},
	}

	for name, tc := range tests {
		if got := domain.LimitSumOK(tc.wallet, tc.accounts); got != tc.want {
			t.Errorf("%s: LimitSumOK = %v, want %v", name, got, tc.want)
		}
	}
}

func TestExceedsAndAvailable(t *testing.T) {
	if domain.Exceeds(nil, 1<<40, 1<<40) {
		t.Error("no limit can be exceeded")
	}
	if domain.Exceeds(ptr(100), 60, 40) {
		t.Error("spending exactly up to the limit is within it")
	}
	if !domain.Exceeds(ptr(100), 60, 41) {
		t.Error("one cent over the limit exceeds it")
	}

	if got := domain.Available(1000, 200, 300); got != 500 {
		t.Errorf("Available = %d, want 500", got)
	}
	if got := domain.Available(100, 200, 300); got != 0 {
		t.Errorf("Available never goes below zero, got %d", got)
	}
}

func TestParseName(t *testing.T) {
	if got, err := domain.ParseName("  Holiday fund "); err != nil || got != "Holiday fund" {
		t.Errorf("ParseName = %q, %v", got, err)
	}
	if _, err := domain.ParseName(strings.Repeat("é", 64)); err != nil {
		t.Errorf("64 characters is allowed, got %v", err)
	}

	for name, bad := range map[string]string{
		"empty": "", "blank": "   ", "too long": strings.Repeat("a", 65), "control char": "a\nb",
	} {
		if _, err := domain.ParseName(bad); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestParseLimitAndParseID(t *testing.T) {
	if _, err := domain.ParseLimit(0); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a limit of 0 is not valid: %v", err)
	}
	if _, err := domain.ParseLimit(-5); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a negative limit is not valid: %v", err)
	}
	if v, err := domain.ParseLimit(1); err != nil || v != 1 {
		t.Errorf("ParseLimit(1) = %d, %v", v, err)
	}

	got, err := domain.ParseID("id", "  3F2504E0-4F89-41D3-9A0C-0305E82C3301 ")
	if err != nil || got != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Errorf("ParseID = %q, %v", got, err)
	}
	for _, bad := range []string{"", "42", "not-a-uuid", "3f2504e0-4f89-41d3-9a0c-0305e82c330", "3f2504e04f8941d39a0c0305e82c3301"} {
		if _, err := domain.ParseID("id", bad); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("ParseID(%q) err = %v, want ErrInvalid", bad, err)
		}
	}
}

func awaiting(now time.Time) domain.Operation {
	expires := now.Add(10 * time.Minute)

	return domain.Operation{
		ID: "op", Kind: domain.KindWithdrawal, AccountID: "a", Amount: 500, Status: domain.StatusAwaitingConfirmation,
		LimitScope: domain.ScopeAccount, ConfirmExpiresAt: &expires, DeadlineAt: now.Add(time.Hour), CreatedAt: now,
	}
}

func TestOperationLifecycle(t *testing.T) {
	now := at(2026, 11, 11, 9)

	op := awaiting(now)
	if op.Expired(now) || !op.Expired(now.Add(10*time.Minute)) {
		t.Error("an awaiting operation expires exactly at its confirmation deadline")
	}

	if err := op.Confirm(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if op.Status != domain.StatusPending || !op.OverLimitConfirmed || op.LimitScope != 0 || op.ConfirmExpiresAt != nil || op.ExecutedAt == nil {
		t.Errorf("after Confirm: %+v", op)
	}
	if err := op.Confirm(now); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("confirming twice: %v", err)
	}

	if err := op.Settle(now); err != nil || op.Status != domain.StatusSettling {
		t.Fatalf("Settle: %v, status %s", err, op.Status)
	}
	if err := op.Settle(now); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("settling a settling operation: %v", err)
	}
	if err := op.Complete(now); err != nil || op.Status != domain.StatusCompleted || op.CompletedAt == nil {
		t.Fatalf("Complete: %v, %+v", err, op)
	}
	if err := op.Fail(domain.FailureRejected, now); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("failing a completed operation: %v", err)
	}
}

func TestOperationFailureRules(t *testing.T) {
	now := at(2026, 11, 11, 9)

	op := awaiting(now)
	if err := op.Complete(now); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an awaiting operation cannot complete: %v", err)
	}
	if err := op.Settle(now); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an awaiting operation cannot settle: %v", err)
	}
	if err := op.Fail(domain.FailureReason(99), now); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an unknown failure reason: %v", err)
	}
	if err := op.Fail(domain.FailureCancelled, now); err != nil {
		t.Fatal(err)
	}
	if op.Status != domain.StatusFailed || op.Failure != domain.FailureCancelled || op.ConfirmExpiresAt != nil || op.CompletedAt == nil {
		t.Errorf("after Fail: %+v", op)
	}
	if err := op.Fail(domain.FailureTimedOut, now); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("failing twice: %v", err)
	}

	pending := domain.Operation{Status: domain.StatusPending}
	if err := pending.Complete(now); err != nil {
		t.Errorf("a pending operation (a credit) can complete directly: %v", err)
	}
}

func TestOperationTouchesAndDeadline(t *testing.T) {
	now := at(2026, 11, 11, 9)
	op := domain.Operation{AccountID: "a", CounterpartyID: "b", DeadlineAt: now}

	if !op.Touches("a") || !op.Touches("b") || op.Touches("c") || (domain.Operation{AccountID: "a"}).Touches("") {
		t.Error("Touches must match either side and never the empty id")
	}
	if !op.PastDeadline(now) || op.PastDeadline(now.Add(-time.Nanosecond)) {
		t.Error("an operation is past its deadline from the deadline on")
	}
}

func TestHoldActive(t *testing.T) {
	now := at(2026, 11, 11, 9)
	hold := domain.Hold{ExpiresAt: now.Add(time.Hour)}

	if !hold.Active(now) {
		t.Error("a fresh hold is active")
	}
	if hold.Active(now.Add(time.Hour)) {
		t.Error("a hold is over at its expiry instant")
	}

	released := now
	if (domain.Hold{ExpiresAt: hold.ExpiresAt, ReleasedAt: &released}).Active(now) {
		t.Error("a released hold is not active")
	}
	if (domain.Hold{ExpiresAt: hold.ExpiresAt, CapturedBy: "op"}).Active(now) {
		t.Error("a captured hold is not active")
	}
}

func TestErrorMessages(t *testing.T) {
	if msg := (&domain.InsufficientFundsError{Available: 5, Requested: 9}).Error(); !strings.Contains(msg, "available 5") || !strings.Contains(msg, "requested 9") {
		t.Errorf("message = %q", msg)
	}

	exp := at(2026, 11, 11, 9)
	msg := (&domain.ConfirmationRequiredError{OperationID: "op-1", Scope: domain.ScopeWallet, ExpiresAt: exp}).Error()
	if !strings.Contains(msg, "wallet") || !strings.Contains(msg, "op-1") || !strings.Contains(msg, "2026-11-11T09:30:00Z") {
		t.Errorf("message = %q", msg)
	}
}
