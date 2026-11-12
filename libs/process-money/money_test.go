package processmoney_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
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
		"the maximum":                {processmoney.MaxAmount, 1_000_000_000_000},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cents, err := processmoney.ToMinorUnits(tc.amount)

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
		"over the maximum":     {processmoney.MaxAmount + 0.01, "must not exceed"},
		"astronomical":         {1e300, "must not exceed"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cents, err := processmoney.ToMinorUnits(tc.amount)

			if !errors.Is(err, processmoney.ErrInvalidAmount) {
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
		if got := processmoney.FromMinorUnits(cents); got != want {
			t.Errorf("FromMinorUnits(%d) = %v, want %v", cents, got, want)
		}
	}
}

func TestEveryWholeCentSurvivesTheTripThroughAFloat(t *testing.T) {
	// Services store cents, but callers speak floats. Any amount a service hands out must come
	// back as exactly the same cents when it is sent again.
	check := func(cents int64) {
		t.Helper()

		back, err := processmoney.ToMinorUnits(processmoney.FromMinorUnits(cents))
		if err != nil || back != cents {
			t.Fatalf("%d cents -> %v -> %d, %v", cents, processmoney.FromMinorUnits(cents), back, err)
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

func TestBalanceToMinorUnitsAcceptsZeroAndNegativeWholeCents(t *testing.T) {
	tests := map[string]struct {
		balance float64
		cents   int64
	}{
		"zero":                 {0, 0},
		"negative zero":        {math.Copysign(0, -1), 0},
		"one cent":             {0.01, 1},
		"typical":              {1234.56, 123456},
		"float noise":          {0.1 + 0.2, 30},
		"negative":             {-2.5, -250},
		"twice the max amount": {processmoney.MaxBalance, 2_000_000_000_000},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cents, err := processmoney.BalanceToMinorUnits(tc.balance)

			if err != nil || cents != tc.cents {
				t.Errorf("BalanceToMinorUnits(%v) = %d, %v; want %d", tc.balance, cents, err, tc.cents)
			}
		})
	}
}

func TestBalanceToMinorUnitsRejectsWhatIsNotWholeCents(t *testing.T) {
	tests := map[string]float64{
		"NaN":               math.NaN(),
		"positive infinity": math.Inf(1),
		"negative infinity": math.Inf(-1),
		"half a cent":       10.005,
		"a tenth of a cent": 0.001,
		"over the maximum":  processmoney.MaxBalance + 1,
		"under the minimum": -processmoney.MaxBalance - 1,
	}

	for name, balance := range tests {
		t.Run(name, func(t *testing.T) {
			cents, err := processmoney.BalanceToMinorUnits(balance)

			if !errors.Is(err, processmoney.ErrInvalidAmount) || cents != 0 {
				t.Errorf("BalanceToMinorUnits(%v) = %d, %v; want 0 and ErrInvalidAmount", balance, cents, err)
			}
		})
	}
}

func TestEveryBalanceSurvivesTheTripThroughAFloat(t *testing.T) {
	check := func(cents int64) {
		t.Helper()

		back, err := processmoney.BalanceToMinorUnits(processmoney.FromMinorUnits(cents))
		if err != nil || back != cents {
			t.Fatalf("%d cents -> %v -> %d, %v", cents, processmoney.FromMinorUnits(cents), back, err)
		}
	}

	for cents := int64(-300_000); cents <= 300_000; cents++ {
		check(cents)
	}

	random := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 500_000; i++ {
		check(random.Int64N(4_000_000_000_001) - 2_000_000_000_000)
	}
	check(2_000_000_000_000)
	check(-2_000_000_000_000)
}
