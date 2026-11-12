package domain_test

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
)

func TestParseIDAcceptsUUIDsAndCanonicalizesThem(t *testing.T) {
	got, err := domain.ParseID("account_id", "  3F2504E0-4F89-41D3-9A0C-0305E82C3301 ")

	if err != nil || got != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Errorf("ParseID = %q, %v; want the lower-case UUID", got, err)
	}
}

func TestParseIDRejectsEverythingElse(t *testing.T) {
	tests := map[string]string{
		"empty":            "",
		"blank":            "   ",
		"too short":        "3f2504e0-4f89-41d3-9a0c",
		"no dashes":        "3f2504e04f8941d39a0c0305e82c3301",
		"not hex":          "zf2504e0-4f89-41d3-9a0c-0305e82c3301",
		"an account name":  "alice",
		"sql":              "'; DROP TABLE ledger_entries; --",
		"trailing garbage": "3f2504e0-4f89-41d3-9a0c-0305e82c3301x",
		"the nil UUID":     "00000000-0000-0000-0000-000000000000",
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := domain.ParseID("account_id", value)

			if !errors.Is(err, domain.ErrInvalidID) {
				t.Fatalf("err = %v, want ErrInvalidID", err)
			}
			if !strings.Contains(err.Error(), "account_id must be a UUID") {
				t.Errorf("err = %q, want it to name the field", err)
			}
		})
	}
}

func TestDirectionSignMakesABalanceTheSumOfSignedAmounts(t *testing.T) {
	if domain.Credit.Sign() != 1 || domain.Debit.Sign() != -1 {
		t.Errorf("signs = %d, %d; want +1 for credit and -1 for debit", domain.Credit.Sign(), domain.Debit.Sign())
	}
}

func TestInsufficientFundsErrorStatesTheBalanceInCurrencyUnits(t *testing.T) {
	var err error = &domain.InsufficientFundsError{Balance: 500, Requested: 1050}

	var insufficient *domain.InsufficientFundsError
	if !errors.As(err, &insufficient) || insufficient.Balance != 500 {
		t.Fatalf("errors.As failed for %v", err)
	}
	if want := "insufficient funds: balance 5.00, requested 10.50"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func TestPeriodBounds(t *testing.T) {
	tests := map[string]struct {
		period     domain.Period
		at         time.Time
		start, end time.Time
	}{
		"month: mid month":                   {domain.PeriodMonth, utc(2026, 11, 9, 12, 34), utc(2026, 11, 1, 0, 0), utc(2026, 12, 1, 0, 0)},
		"month: first instant is inclusive":  {domain.PeriodMonth, utc(2026, 11, 1, 0, 0), utc(2026, 11, 1, 0, 0), utc(2026, 12, 1, 0, 0)},
		"month: the end belongs to the next": {domain.PeriodMonth, utc(2026, 12, 1, 0, 0), utc(2026, 12, 1, 0, 0), utc(2027, 1, 1, 0, 0)},
		"month: december rolls the year":     {domain.PeriodMonth, utc(2026, 12, 31, 23, 59), utc(2026, 12, 1, 0, 0), utc(2027, 1, 1, 0, 0)},
		"month: leap february":               {domain.PeriodMonth, utc(2028, 2, 29, 8, 0), utc(2028, 2, 1, 0, 0), utc(2028, 3, 1, 0, 0)},
		"month: ordinary february":           {domain.PeriodMonth, utc(2027, 2, 28, 8, 0), utc(2027, 2, 1, 0, 0), utc(2027, 3, 1, 0, 0)},

		"quarter: january":  {domain.PeriodQuarter, utc(2026, 1, 1, 0, 0), utc(2026, 1, 1, 0, 0), utc(2026, 4, 1, 0, 0)},
		"quarter: march":    {domain.PeriodQuarter, utc(2026, 3, 31, 23, 59), utc(2026, 1, 1, 0, 0), utc(2026, 4, 1, 0, 0)},
		"quarter: april":    {domain.PeriodQuarter, utc(2026, 4, 1, 0, 0), utc(2026, 4, 1, 0, 0), utc(2026, 7, 1, 0, 0)},
		"quarter: august":   {domain.PeriodQuarter, utc(2026, 8, 15, 0, 0), utc(2026, 7, 1, 0, 0), utc(2026, 10, 1, 0, 0)},
		"quarter: november": {domain.PeriodQuarter, utc(2026, 11, 9, 0, 0), utc(2026, 10, 1, 0, 0), utc(2027, 1, 1, 0, 0)},

		"year: mid year":    {domain.PeriodYear, utc(2026, 6, 15, 0, 0), utc(2026, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0)},
		"year: last minute": {domain.PeriodYear, utc(2026, 12, 31, 23, 59), utc(2026, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0)},

		"five_year: a multiple of five":     {domain.PeriodFiveYear, utc(2030, 1, 1, 0, 0), utc(2030, 1, 1, 0, 0), utc(2035, 1, 1, 0, 0)},
		"five_year: right after":            {domain.PeriodFiveYear, utc(2031, 5, 5, 0, 0), utc(2030, 1, 1, 0, 0), utc(2035, 1, 1, 0, 0)},
		"five_year: last year of the block": {domain.PeriodFiveYear, utc(2029, 12, 31, 23, 59), utc(2025, 1, 1, 0, 0), utc(2030, 1, 1, 0, 0)},
		"five_year: today's block":          {domain.PeriodFiveYear, utc(2026, 11, 9, 0, 0), utc(2025, 1, 1, 0, 0), utc(2030, 1, 1, 0, 0)},
		"five_year: just before 2025":       {domain.PeriodFiveYear, utc(2024, 12, 31, 23, 59), utc(2020, 1, 1, 0, 0), utc(2025, 1, 1, 0, 0)},
		"five_year: year 2000":              {domain.PeriodFiveYear, utc(2000, 3, 3, 0, 0), utc(2000, 1, 1, 0, 0), utc(2005, 1, 1, 0, 0)},
		"five_year: year 1999":              {domain.PeriodFiveYear, utc(1999, 3, 3, 0, 0), utc(1995, 1, 1, 0, 0), utc(2000, 1, 1, 0, 0)},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			start, end := tc.period.Bounds(tc.at)

			if !start.Equal(tc.start) || !end.Equal(tc.end) {
				t.Errorf("Bounds(%v) = [%v, %v), want [%v, %v)", tc.at, start, end, tc.start, tc.end)
			}
		})
	}
}

func TestPeriodBoundsAreComputedInUTC(t *testing.T) {
	// 01:00 on 1 December in UTC+2 is still 23:00 on 30 November in UTC.
	local := time.Date(2026, 12, 1, 1, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))

	start, end := domain.PeriodMonth.Bounds(local)

	if !start.Equal(utc(2026, 11, 1, 0, 0)) || !end.Equal(utc(2026, 12, 1, 0, 0)) {
		t.Errorf("Bounds = [%v, %v), want November in UTC", start, end)
	}
	if start.Location() != time.UTC || end.Location() != time.UTC {
		t.Errorf("bounds are in %v and %v, want UTC", start.Location(), end.Location())
	}
}

func TestPeriodsTileTimeAndNest(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 4))

	for i := 0; i < 20_000; i++ {
		at := time.Unix(random.Int64N(4_000_000_000), random.Int64N(1_000_000_000)).UTC()
		var previousStart, previousEnd time.Time

		for _, period := range domain.AllPeriods {
			start, end := period.Bounds(at)

			if at.Before(start) || !at.Before(end) {
				t.Fatalf("%s: %v is not inside [%v, %v)", period, at, start, end)
			}
			if nextStart, _ := period.Bounds(end); !nextStart.Equal(end) {
				t.Fatalf("%s: the period after %v starts at %v, want no gap or overlap", period, end, nextStart)
			}
			if !previousStart.IsZero() && (start.After(previousStart) || end.Before(previousEnd)) {
				t.Fatalf("%s [%v, %v) does not contain the finer period [%v, %v)", period, start, end, previousStart, previousEnd)
			}
			previousStart, previousEnd = start, end
		}
	}
}

func TestParsePeriod(t *testing.T) {
	for _, period := range domain.AllPeriods {
		got, err := domain.ParsePeriod(string(period))
		if err != nil || got != period {
			t.Errorf("ParsePeriod(%q) = %q, %v", period, got, err)
		}
	}

	for _, bad := range []string{"", "week", "Month", "5y", "five-year"} {
		if _, err := domain.ParsePeriod(bad); !errors.Is(err, domain.ErrInvalidPeriod) {
			t.Errorf("ParsePeriod(%q) err = %v, want ErrInvalidPeriod", bad, err)
		}
	}
}
