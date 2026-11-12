package domain

import "time"

// StartOfDay returns the UTC midnight at or before t: the start of the day the daily limits
// count from.
func StartOfDay(t time.Time) time.Time {
	t = t.UTC()

	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// IsWorkingDay reports whether t falls on a Monday to Friday, in UTC. Public holidays are not
// modelled.
func IsWorkingDay(t time.Time) bool {
	switch t.UTC().Weekday() {
	case time.Saturday, time.Sunday:
		return false
	default:
		return true
	}
}

// WorkingDaysAfter returns the moment n working days after t: the same time of day, n Monday-to-
// Friday days later, in UTC. Zero returns t itself. A start on a weekend counts from the next
// working day, so Saturday + 1 working day is Monday, and Friday + 1 is Monday.
func WorkingDaysAfter(t time.Time, n int) time.Time {
	t = t.UTC()

	for added := 0; added < n; {
		t = t.AddDate(0, 0, 1)
		if IsWorkingDay(t) {
			added++
		}
	}

	return t
}
