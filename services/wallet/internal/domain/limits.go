package domain

// LimitSumOK reports whether a wallet limit and its accounts' limits can stand together: the
// accounts' limits, the ones that are set, must not add up to more than the wallet limit. An
// account without a limit is bounded only by the wallet limit, and a wallet without one imposes
// no sum rule. Limits are cents.
func LimitSumOK(walletLimit *int64, accountLimits []*int64) bool {
	if walletLimit == nil {
		return true
	}

	var sum int64
	for _, limit := range accountLimits {
		if limit != nil {
			sum += *limit
		}
	}

	return sum <= *walletLimit
}

// Exceeds reports whether spending amount on top of spent would go over limit. A nil limit is no
// limit at all.
func Exceeds(limit *int64, spent, amount int64) bool {
	return limit != nil && spent+amount > *limit
}

// Available is what can be spent now, in cents: the ledger balance, less money reserved by holds,
// less outgoing operations whose debit has not reached the ledger yet. It never goes below zero.
func Available(balance, held, pendingOut int64) int64 {
	return max(balance-held-pendingOut, 0)
}
