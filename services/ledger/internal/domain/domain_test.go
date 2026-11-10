package domain_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
)

func TestToMinorUnitsMultipliesByOneHundred(t *testing.T) {
	tests := map[string]struct {
		amount float64
		cents  int64
	}{
		"one cent":                   {0.01, 1},
		"ten cents":                  {0.1, 10},
		"seven cents":                {0.07, 7},
		"whole unit":                 {1, 100},
		"typical price":              {19.99, 1999},
		"1.15 is 114.99999999999999": {1.15, 115},
		"8.2 is not exact in binary": {8.2, 820},
		"float noise: 0.1 + 0.2":     {0.1 + 0.2, 30},
		"large":                      {1234567.89, 123456789},
		"just under the maximum":     {9999999999.99, 999999999999},
		"the maximum":                {domain.MaxAmount, 1_000_000_000_000},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cents, err := domain.ToMinorUnits(tc.amount)

			if err != nil || cents != tc.cents {
				t.Errorf("ToMinorUnits(%v) = %d, %v; want %d", tc.amount, cents, err, tc.cents)
			}
		})
	}
}

func TestToMinorUnitsRejectsAmountsThatAreNotWholeCents(t *testing.T) {
	tests := map[string]struct {
		amount  float64
		message string
	}{
		"zero":                 {0, "greater than 0"},
		"negative":             {-5, "greater than 0"},
		"negative zero":        {math.Copysign(0, -1), "greater than 0"},
		"NaN":                  {math.NaN(), "finite"},
		"positive infinity":    {math.Inf(1), "finite"},
		"negative infinity":    {math.Inf(-1), "finite"},
		"half a cent too many": {10.005, "2 decimal places"},
		"a tenth of a cent":    {0.001, "2 decimal places"},
		"three decimals":       {1.234, "2 decimal places"},
		"far below one cent":   {0.00001, "at least 0.01"},
		"over the maximum":     {domain.MaxAmount + 0.01, "must not exceed"},
		"astronomical":         {1e300, "must not exceed"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cents, err := domain.ToMinorUnits(tc.amount)

			if !errors.Is(err, domain.ErrInvalidAmount) {
				t.Fatalf("ToMinorUnits(%v) = %d, %v; want ErrInvalidAmount", tc.amount, cents, err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %q, want it to contain %q", err, tc.message)
			}
			if cents != 0 {
				t.Errorf("cents = %d, want 0 alongside an error", cents)
			}
		})
	}
}

func TestFromMinorUnitsDividesByOneHundred(t *testing.T) {
	tests := map[int64]float64{
		0:                 0,
		1:                 0.01,
		5:                 0.05,
		100:               1,
		1999:              19.99,
		123456789:         1234567.89,
		1_000_000_000_000: 10_000_000_000,
		-250:              -2.5,
	}

	for cents, want := range tests {
		if got := domain.FromMinorUnits(cents); got != want {
			t.Errorf("FromMinorUnits(%d) = %v, want %v", cents, got, want)
		}
	}
}

func TestEveryWholeCentSurvivesTheTripThroughAFloat(t *testing.T) {
	// The ledger stores cents, but callers speak floats. Any amount the ledger hands out must come
	// back as exactly the same cents when it is posted again.
	check := func(cents int64) {
		t.Helper()

		back, err := domain.ToMinorUnits(domain.FromMinorUnits(cents))
		if err != nil || back != cents {
			t.Fatalf("%d cents -> %v -> %d, %v", cents, domain.FromMinorUnits(cents), back, err)
		}
	}

	for cents := int64(1); cents <= 300_000; cents++ {
		check(cents)
	}

	random := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 500_000; i++ {
		check(1 + random.Int64N(1_000_000_000_000))
	}
	check(1_000_000_000_000)
}

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
