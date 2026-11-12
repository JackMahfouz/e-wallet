package httpapi

import (
	"bytes"
	"encoding/json"
	"time"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// Amounts cross this boundary as JSON numbers (12.34) and are cents on the other side of it.

func amountOut(cents int64) float64 { return processmoney.FromMinorUnits(cents) }

func limitOut(limit *int64) *float64 {
	if limit == nil {
		return nil
	}
	v := amountOut(*limit)

	return &v
}

func timeOut(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func timePtrOut(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := timeOut(*t)

	return &s
}

type accountJSON struct {
	ID         string   `json:"id"`
	WalletID   string   `json:"wallet_id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	DailyLimit *float64 `json:"daily_limit"`
	Balance    float64  `json:"balance"`
	Held       float64  `json:"held"`
	Available  float64  `json:"available"`
	SpentToday float64  `json:"spent_today"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	ClosedAt   *string  `json:"closed_at,omitempty"`
}

func accountOut(a application.AccountView) accountJSON {
	return accountJSON{
		ID: a.ID, WalletID: a.WalletID, Name: a.Name, Status: a.Status.String(), DailyLimit: limitOut(a.DailyLimit),
		Balance: amountOut(a.Balance), Held: amountOut(a.Held), Available: amountOut(a.Available),
		SpentToday: amountOut(a.SpentToday), CreatedAt: timeOut(a.CreatedAt), UpdatedAt: timeOut(a.UpdatedAt),
		ClosedAt: timePtrOut(a.ClosedAt),
	}
}

type walletJSON struct {
	ID         string        `json:"id"`
	UserID     string        `json:"user_id"`
	DailyLimit *float64      `json:"daily_limit"`
	SpentToday float64       `json:"spent_today"`
	Accounts   []accountJSON `json:"accounts"`
}

func walletOut(w application.WalletView) walletJSON {
	out := walletJSON{
		ID: w.ID, UserID: w.UserID, DailyLimit: limitOut(w.DailyLimit), SpentToday: amountOut(w.SpentToday),
		Accounts: make([]accountJSON, 0, len(w.Accounts)),
	}
	for _, a := range w.Accounts {
		out.Accounts = append(out.Accounts, accountOut(a))
	}

	return out
}

type confirmationJSON struct {
	LimitScope string `json:"limit_scope"`
	ExpiresAt  string `json:"expires_at"`
}

type operationJSON struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Status         string            `json:"status"`
	FailureReason  string            `json:"failure_reason,omitempty"`
	AccountID      string            `json:"account_id"`
	CounterpartyID string            `json:"counterparty_account_id,omitempty"`
	Amount         float64           `json:"amount"`
	Spending       bool              `json:"spending"`
	Confirmation   *confirmationJSON `json:"confirmation,omitempty"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	DeadlineAt     string            `json:"deadline_at"`
	CompletedAt    *string           `json:"completed_at,omitempty"`
}

func operationOut(op domain.Operation) operationJSON {
	out := operationJSON{
		ID: op.ID, Kind: op.Kind.String(), Status: op.Status.String(), AccountID: op.AccountID,
		CounterpartyID: op.CounterpartyID, Amount: amountOut(op.Amount), Spending: op.Spending,
		CreatedAt: timeOut(op.CreatedAt), UpdatedAt: timeOut(op.UpdatedAt), DeadlineAt: timeOut(op.DeadlineAt),
		CompletedAt: timePtrOut(op.CompletedAt),
	}
	if op.Failure != 0 {
		out.FailureReason = op.Failure.String()
	}
	if op.Status == domain.StatusAwaitingConfirmation && op.ConfirmExpiresAt != nil {
		out.Confirmation = &confirmationJSON{LimitScope: op.LimitScope.String(), ExpiresAt: timeOut(*op.ConfirmExpiresAt)}
	}

	return out
}

// optionalAmount is a JSON field that can be absent (leave it), null (clear it) or a number (set
// it). The decoder only calls UnmarshalJSON for a field that is present.
type optionalAmount struct {
	Set   bool
	Value *float64
}

func (o *optionalAmount) UnmarshalJSON(data []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.Value = nil

		return nil
	}

	var v float64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.Value = &v

	return nil
}

// change turns the field into a LimitChange, with the amount cast to cents.
func (o optionalAmount) change() (application.LimitChange, error) {
	if !o.Set {
		return application.LimitChange{}, nil
	}
	if o.Value == nil {
		return application.LimitChange{Set: true}, nil
	}

	cents, err := processmoney.ToMinorUnits(*o.Value)
	if err != nil {
		return application.LimitChange{}, err
	}

	return application.LimitChange{Set: true, Value: &cents}, nil
}

type createAccountBody struct {
	Name       string          `json:"name"`
	DailyLimit *optionalAmount `json:"daily_limit"`
}

type patchAccountBody struct {
	Name       *string        `json:"name"`
	DailyLimit optionalAmount `json:"daily_limit"`
	Status     *string        `json:"status"`
}

type walletPatchBody struct {
	DailyLimit optionalAmount `json:"daily_limit"`
}

type amountBody struct {
	Amount *float64 `json:"amount"`
}

type transferBody struct {
	FromAccountID string   `json:"from_account_id"`
	ToAccountID   string   `json:"to_account_id"`
	Amount        *float64 `json:"amount"`
}

type confirmBody struct {
	AcknowledgeOverLimit bool `json:"acknowledge_over_limit"`
}
