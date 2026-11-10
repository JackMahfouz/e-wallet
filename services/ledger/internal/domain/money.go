// Package domain holds the ledger's vocabulary: money, entries, checkpoints and the rules that
// need no infrastructure. It has no database or transport imports.
package domain

import (
	"errors"
	"fmt"
	"math"
)

const (
	// MaxAmount is the largest single posting, in whole currency units. It keeps every amount
	// well inside float64's exact range, so a value with two decimals survives the trip to cents.
	MaxAmount = 10_000_000_000.0

	minorUnitsPerUnit = 100

	// centTolerance is how far, in cents, an amount may sit from a whole cent and still count as
	// one. It absorbs float noise (0.1+0.2 is 0.30000000000000004) but not real extra decimals
	// (10.005 is half a cent away).
	centTolerance = 1e-3
)

// ErrInvalidAmount is wrapped by every amount validation failure.
var ErrInvalidAmount = errors.New("invalid amount")

// ToMinorUnits converts a posted amount to whole cents (amount x 100), the form in which every
// amount is stored and summed. Floats exist only at the edge: nothing past this point does
// float arithmetic on money.
//
// The amount must be finite, positive, at most MaxAmount, and a whole number of cents. It is never
// silently rounded to a different value: 10.005 is rejected rather than stored as 10.01.
func ToMinorUnits(amount float64) (int64, error) {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0):
		return 0, fmt.Errorf("%w: must be a finite number", ErrInvalidAmount)
	case amount <= 0:
		return 0, fmt.Errorf("%w: must be greater than 0", ErrInvalidAmount)
	case amount > MaxAmount:
		return 0, fmt.Errorf("%w: must not exceed %.0f", ErrInvalidAmount, MaxAmount)
	}

	scaled := amount * minorUnitsPerUnit
	cents := math.Round(scaled)

	if math.Abs(scaled-cents) > centTolerance {
		return 0, fmt.Errorf("%w: must have at most 2 decimal places", ErrInvalidAmount)
	}
	if cents < 1 {
		return 0, fmt.Errorf("%w: must be at least 0.01", ErrInvalidAmount)
	}

	return int64(cents), nil
}

// FromMinorUnits converts stored cents back to an amount (cents / 100) for reads.
func FromMinorUnits(cents int64) float64 {
	return float64(cents) / minorUnitsPerUnit
}
