package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
)

var (
	// ErrInvalidID is wrapped by every identifier validation failure.
	ErrInvalidID = errors.New("invalid id")

	// ErrTransactionConflict means a transaction id was already posted to this account in this
	// direction, but with a different amount.
	ErrTransactionConflict = errors.New("transaction already posted with a different amount")
)

// InsufficientFundsError is returned when a debit is larger than the account's balance. An empty
// account has a balance of zero, so every debit against it is refused.
type InsufficientFundsError struct {
	Balance   int64 // cents
	Requested int64 // cents
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient funds: balance %.2f, requested %.2f", processmoney.FromMinorUnits(e.Balance), processmoney.FromMinorUnits(e.Requested))
}

// Direction says whether an entry adds to an account (credit) or takes from it (debit).
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Sign is +1 for a credit and -1 for a debit, so a balance is the sum of sign x amount.
func (d Direction) Sign() int64 {
	if d == Credit {
		return 1
	}

	return -1
}

// Entry is one immutable line of an account's ledger. Entries are only ever appended.
type Entry struct {
	ID            string
	AccountID     string
	TransactionID string
	// Seq numbers an account's entries 1, 2, 3, ... with no gaps. It is the ledger's true order.
	Seq       int64
	Direction Direction
	Amount    int64 // cents, always positive
	CreatedAt time.Time
}

// NewEntry is an entry about to be appended; the database assigns the rest.
type NewEntry struct {
	AccountID     string
	TransactionID string
	Direction     Direction
	Amount        int64 // cents
}

// AccountBalance is an account's balance and how far its ledger goes.
type AccountBalance struct {
	Balance int64 // cents
	LastSeq int64 // seq of the newest entry; 0 when the account has no entries
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const nilUUID = "00000000-0000-0000-0000-000000000000"

// ParseID validates a UUID and returns it in canonical lower case. field names the value in the
// error ("account_id").
func ParseID(field, value string) (string, error) {
	canonical := strings.ToLower(strings.TrimSpace(value))

	if !uuidPattern.MatchString(canonical) || canonical == nilUUID {
		return "", fmt.Errorf("%w: %s must be a UUID", ErrInvalidID, field)
	}

	return canonical, nil
}
