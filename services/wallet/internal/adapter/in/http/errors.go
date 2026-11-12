package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// errorBody is every error response: a message for people, a code for programs.
type errorBody struct {
	Message string `json:"message"`
	Code    string `json:"code"`

	// Set with confirmation_required: the saved operation waiting to be confirmed.
	Operation *operationJSON `json:"operation,omitempty"`
}

// classify maps an error from the core to a status and body. Messages for the caller's own
// mistakes come from the core and are safe to show; anything unexpected is reported generically.
func classify(err error) (int, errorBody, bool) {
	var (
		insufficient *domain.InsufficientFundsError
		confirmation *domain.ConfirmationRequiredError
	)

	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		return http.StatusUnauthorized, errorBody{Message: "a valid bearer token is required", Code: "unauthenticated"}, false
	case errors.Is(err, domain.ErrOTPInvalid):
		return http.StatusForbidden, errorBody{Message: "the one-time code (X-OTP header) is missing or wrong", Code: "otp_invalid"}, false
	case errors.Is(err, domain.ErrFundingDisabled):
		return http.StatusForbidden, errorBody{Message: "deposits and withdrawals are not enabled", Code: "funding_disabled"}, false
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, errorBody{Message: "not found", Code: "not_found"}, false
	case errors.Is(err, processmoney.ErrInvalidAmount):
		return http.StatusBadRequest, errorBody{Message: trimPrefix(err, "invalid amount: "), Code: "invalid_amount"}, false
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest, errorBody{Message: trimPrefix(err, domain.ErrInvalid.Error()+": "), Code: "invalid_request"}, false
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		return http.StatusConflict, errorBody{Message: err.Error(), Code: "idempotency_key_reused"}, false
	case errors.As(err, &confirmation):
		return http.StatusConflict, errorBody{Message: confirmation.Error(), Code: "confirmation_required"}, false
	case errors.Is(err, domain.ErrAccountNotActive):
		return http.StatusConflict, errorBody{Message: trimPrefix(err, domain.ErrAccountNotActive.Error()+": "), Code: "account_not_active"}, false
	case errors.Is(err, domain.ErrLimitSum):
		return http.StatusConflict, errorBody{Message: trimPrefix(err, domain.ErrLimitSum.Error()+": "), Code: "limit_exceeds_wallet_limit"}, false
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, errorBody{Message: trimPrefix(err, domain.ErrConflict.Error()+": "), Code: "conflict"}, false
	case errors.As(err, &insufficient):
		return http.StatusUnprocessableEntity, errorBody{
			Message: fmt.Sprintf("insufficient funds: %.2f available, %.2f requested",
				processmoney.FromMinorUnits(insufficient.Available), processmoney.FromMinorUnits(insufficient.Requested)),
			Code: "insufficient_funds",
		}, false
	case errors.Is(err, domain.ErrUnavailable):
		return http.StatusServiceUnavailable, errorBody{Message: "a service this depends on is not available; try again shortly", Code: "unavailable"}, false
	default:
		return http.StatusInternalServerError, errorBody{Message: "internal error", Code: "internal_error"}, true
	}
}

// trimPrefix drops the sentinel's own text from a wrapped error, leaving the explanation.
func trimPrefix(err error, prefix string) string {
	return strings.TrimPrefix(err.Error(), prefix)
}
