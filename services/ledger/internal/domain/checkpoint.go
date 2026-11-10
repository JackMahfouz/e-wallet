package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidPeriod is wrapped when a checkpoint period is not one of the supported ones.
var ErrInvalidPeriod = errors.New("invalid period")

// Period is how often a checkpoint is taken.
type Period string

const (
	PeriodMonth    Period = "month"
	PeriodQuarter  Period = "quarter"
	PeriodYear     Period = "year"
	PeriodFiveYear Period = "five_year"
)

// AllPeriods lists every checkpoint period, finest first.
var AllPeriods = []Period{PeriodMonth, PeriodQuarter, PeriodYear, PeriodFiveYear}

// ParsePeriod validates a period name.
func ParsePeriod(name string) (Period, error) {
	for _, period := range AllPeriods {
		if string(period) == name {
			return period, nil
		}
	}

	return "", fmt.Errorf("%w: %q is not one of month, quarter, year, five_year", ErrInvalidPeriod, name)
}

// Bounds returns the period containing t as a half-open interval [start, end), in UTC.
//
//	month      the calendar month
//	quarter    Jan-Mar, Apr-Jun, Jul-Sep or Oct-Dec
//	year       the calendar year
//	five_year  blocks of five calendar years aligned to multiples of five: 2025-2029, 2030-2034, ...
func (p Period) Bounds(t time.Time) (start, end time.Time) {
	t = t.UTC()
	year, month := t.Year(), int(t.Month())

	switch p {
	case PeriodMonth:
		start = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	case PeriodQuarter:
		start = time.Date(year, time.Month((month-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 3, 0)
	case PeriodYear:
		start = time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(1, 0, 0)
	case PeriodFiveYear:
		start = time.Date(floorDiv(year, 5)*5, time.January, 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(5, 0, 0)
	default:
		panic("domain: unknown period " + string(p))
	}

	return start, end
}

// floorDiv divides rounding towards negative infinity, so five-year blocks stay aligned for any year.
func floorDiv(a, b int) int {
	quotient := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		quotient--
	}

	return quotient
}

// Checkpoint is an account's closing balance for one finished period. Like entries, checkpoints
// are only ever appended.
//
// A checkpoint is a verified shortcut: Balance is the account's balance after entry LastSeq, so
// the current balance is the latest checkpoint plus the entries after LastSeq.
type Checkpoint struct {
	ID          string
	AccountID   string
	Period      Period
	PeriodStart time.Time
	PeriodEnd   time.Time // exclusive
	LastSeq     int64
	Balance     int64 // cents
	CreatedAt   time.Time
}
