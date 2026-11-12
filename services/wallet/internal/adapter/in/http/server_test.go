package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpapi "github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/in/http"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/testsupport"
)

type api struct {
	*testsupport.Env
	t      *testing.T
	server *httptest.Server
	ada    string
	bob    string
	keys   int
}

func newAPI(t *testing.T, mutate ...func(*application.Options)) *api {
	t.Helper()

	env := testsupport.NewEnv(t, mutate...)
	a := &api{Env: env, t: t, ada: testsupport.UUID(1), bob: testsupport.UUID(2)}
	env.Tokens.Add("ada-token", a.ada)
	env.Tokens.Add("bob-token", a.bob)

	a.server = httptest.NewServer(httpapi.New(env.Svc, slog.New(slog.DiscardHandler)))
	t.Cleanup(a.server.Close)

	return a
}

type response struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

// call makes a request. headers are name, value pairs.
func (a *api) call(method, path string, body any, headers ...string) response {
	a.t.Helper()

	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, a.server.URL+path, reader)
	if err != nil {
		a.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, raw: string(raw), header: resp.Header}
	_ = json.Unmarshal(raw, &out.body)

	return out
}

// as makes a request as ada (or bob) with the one-time code.
func (a *api) asAda(method, path string, body any, extra ...string) response {
	a.t.Helper()

	return a.call(method, path, body, append([]string{"Authorization", "Bearer ada-token", "X-OTP", testsupport.ValidOTP}, extra...)...)
}

func (a *api) asBob(method, path string, body any, extra ...string) response {
	a.t.Helper()

	return a.call(method, path, body, append([]string{"Authorization", "Bearer bob-token", "X-OTP", testsupport.ValidOTP}, extra...)...)
}

// money makes a money POST with a fresh idempotency key.
func (a *api) money(user, path string, body any, extra ...string) response {
	a.t.Helper()

	a.keys++
	key := testsupport.UUID(900_000 + a.keys)
	headers := append([]string{"Idempotency-Key", key}, extra...)

	if user == "bob" {
		return a.asBob("POST", path, body, headers...)
	}

	return a.asAda("POST", path, body, headers...)
}

func (a *api) account(user, name string, balanceCents int64) string {
	a.t.Helper()

	var r response
	if user == "bob" {
		r = a.asBob("POST", "/v1/accounts", map[string]any{"name": name})
	} else {
		r = a.asAda("POST", "/v1/accounts", map[string]any{"name": name})
	}
	if r.status != http.StatusCreated {
		a.t.Fatalf("create account: %d %s", r.status, r.raw)
	}

	id := r.body["id"].(string)
	if balanceCents > 0 {
		a.Ledger.Fund(id, balanceCents)
	}

	return id
}

func wantStatus(t *testing.T, r response, status int, code string) {
	t.Helper()

	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.raw)
	}
	if code != "" && r.body["code"] != code {
		t.Fatalf("code = %v, want %q: %s", r.body["code"], code, r.raw)
	}
}

func TestHealthNeedsNoAuthentication(t *testing.T) {
	a := newAPI(t)

	r := a.call("GET", "/health", nil)
	wantStatus(t, r, 200, "")
	if r.body["status"] != "ok" {
		t.Errorf("body = %s", r.raw)
	}
}

func TestEveryRouteNeedsAValidBearerToken(t *testing.T) {
	a := newAPI(t)
	id := testsupport.UUID(5)

	routes := map[string]string{
		"GET /v1/wallets/me":                       "",
		"PATCH /v1/wallets/me":                     "{}",
		"POST /v1/accounts":                        `{"name":"x"}`,
		"GET /v1/accounts/" + id:                   "",
		"PATCH /v1/accounts/" + id:                 "{}",
		"DELETE /v1/accounts/" + id:                "",
		"GET /v1/accounts/" + id + "/transactions": "",
		"POST /v1/accounts/" + id + "/deposits":    `{"amount":1}`,
		"POST /v1/accounts/" + id + "/withdrawals": `{"amount":1}`,
		"POST /v1/transfers":                       `{}`,
		"GET /v1/operations/" + id:                 "",
		"POST /v1/operations/" + id + "/confirm":   "{}",
		"POST /v1/operations/" + id + "/cancel":    "",
	}

	for route, body := range routes {
		method, path, _ := strings.Cut(route, " ")

		for name, headers := range map[string][]string{
			"no header":     nil,
			"wrong scheme":  {"Authorization", "Basic ada-token"},
			"unknown token": {"Authorization", "Bearer nobody"},
			"empty bearer":  {"Authorization", "Bearer "},
		} {
			var payload any
			if body != "" {
				payload = body
			}

			r := a.call(method, path, payload, headers...)
			if r.status != http.StatusUnauthorized || r.body["code"] != "unauthenticated" {
				t.Errorf("%s (%s): status %d, body %s; want 401 unauthenticated", route, name, r.status, r.raw)
			}
		}
	}
}

func TestTheCallerIsTheTokenNotTheXUserIDHeader(t *testing.T) {
	a := newAPI(t)
	a.account("ada", "ada's", 0)
	bobs := a.account("bob", "bob's", 0)

	// Ada's token with Bob's id in X-User-ID: still Ada, so Bob's account is not found.
	r := a.call("GET", "/v1/accounts/"+bobs, nil, "Authorization", "Bearer ada-token", "X-User-ID", a.bob)
	wantStatus(t, r, 404, "not_found")

	// And no token at all with a valid X-User-ID header is no way in.
	r = a.call("GET", "/v1/wallets/me", nil, "X-User-ID", a.ada)
	wantStatus(t, r, 401, "unauthenticated")
}

func TestAnUnreachableAuthServiceIs503NotATokenError(t *testing.T) {
	a := newAPI(t)
	a.Tokens.Down = true

	wantStatus(t, a.asAda("GET", "/v1/wallets/me", nil), 503, "unavailable")
}

func TestUnknownRoutesAndWrongMethodsAnswerInJSON(t *testing.T) {
	a := newAPI(t)

	r := a.call("GET", "/v1/nothing", nil)
	wantStatus(t, r, 404, "not_found")
	if !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Errorf("content type = %q", r.header.Get("Content-Type"))
	}

	r = a.call("PUT", "/v1/transfers", nil)
	wantStatus(t, r, 405, "method_not_allowed")
	if r.header.Get("Allow") == "" {
		t.Error("a 405 must say which methods are allowed")
	}
}

func TestAccountsAreCreatedReadAndRenamed(t *testing.T) {
	a := newAPI(t)

	created := a.asAda("POST", "/v1/accounts", map[string]any{"name": " Holiday ", "daily_limit": 250.5})
	wantStatus(t, created, 201, "")
	id := created.body["id"].(string)
	if created.body["name"] != "Holiday" || created.body["status"] != "active" || created.body["daily_limit"] != 250.5 || created.body["balance"] != 0.0 {
		t.Errorf("created = %s", created.raw)
	}

	a.Ledger.Fund(id, 1234)
	got := a.asAda("GET", "/v1/accounts/"+id, nil)
	wantStatus(t, got, 200, "")
	if got.body["balance"] != 12.34 || got.body["available"] != 12.34 || got.body["held"] != 0.0 {
		t.Errorf("amounts must come back as decimals: %s", got.raw)
	}

	renamed := a.call("PATCH", "/v1/accounts/"+id, map[string]any{"name": "Trip"}, "Authorization", "Bearer ada-token") // no code needed
	wantStatus(t, renamed, 200, "")
	if renamed.body["name"] != "Trip" {
		t.Errorf("renamed = %s", renamed.raw)
	}

	wallet := a.asAda("GET", "/v1/wallets/me", nil)
	wantStatus(t, wallet, 200, "")
	if accounts, _ := wallet.body["accounts"].([]any); len(accounts) != 1 || wallet.body["user_id"] != a.ada {
		t.Errorf("wallet = %s", wallet.raw)
	}
}

func TestRequestBodiesAreCheckedStrictly(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 1000)
	other := a.account("bob", "b", 0)
	key := []string{"Idempotency-Key", testsupport.UUID(777)}

	tests := map[string]struct {
		method, path string
		body         any
		code         string
		status       int
	}{
		"malformed json":      {"POST", "/v1/accounts", "{not json", "invalid_request", 400},
		"unknown field":       {"POST", "/v1/accounts", `{"name":"x","nmae":"typo"}`, "invalid_request", 400},
		"wrong type":          {"POST", "/v1/accounts", `{"name":5}`, "invalid_request", 400},
		"trailing data":       {"POST", "/v1/accounts", `{"name":"x"} {"name":"y"}`, "invalid_request", 400},
		"empty name":          {"POST", "/v1/accounts", map[string]any{"name": ""}, "invalid_request", 400},
		"zero limit":          {"POST", "/v1/accounts", map[string]any{"name": "x", "daily_limit": 0}, "invalid_amount", 400},
		"three decimals":      {"POST", "/v1/accounts/" + id + "/withdrawals", map[string]any{"amount": 1.234}, "invalid_amount", 400},
		"negative amount":     {"POST", "/v1/accounts/" + id + "/withdrawals", map[string]any{"amount": -5}, "invalid_amount", 400},
		"missing amount":      {"POST", "/v1/accounts/" + id + "/withdrawals", map[string]any{}, "invalid_request", 400},
		"amount over the max": {"POST", "/v1/accounts/" + id + "/deposits", map[string]any{"amount": 1e12}, "invalid_amount", 400},
		"amount as a string":  {"POST", "/v1/accounts/" + id + "/deposits", `{"amount":"5"}`, "invalid_request", 400},
		"transfer to nothing": {"POST", "/v1/transfers", map[string]any{"from_account_id": id, "amount": 5}, "invalid_request", 400},
		"transfer to self":    {"POST", "/v1/transfers", map[string]any{"from_account_id": id, "to_account_id": id, "amount": 5}, "invalid_request", 400},
		"bad id in the path":  {"GET", "/v1/accounts/42", nil, "invalid_request", 400},
		"bad status":          {"PATCH", "/v1/accounts/" + id, map[string]any{"status": "closed"}, "invalid_request", 400},
		"bad limit type":      {"PATCH", "/v1/wallets/me", `{"daily_limit":"lots"}`, "invalid_request", 400},
		"huge body":           {"POST", "/v1/accounts", `{"name":"` + strings.Repeat("x", 70_000) + `"}`, "body_too_large", 413},
	}
	_ = other

	for name, tc := range tests {
		r := a.asAda(tc.method, tc.path, tc.body, key...)
		if r.status != tc.status || r.body["code"] != tc.code {
			t.Errorf("%s: status %d code %v (%s), want %d %s", name, r.status, r.body["code"], r.raw, tc.status, tc.code)
		}
	}
}

func TestLimitsAreSetAndClearedWithANullAndNeedACode(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 0)

	// Without the code, nothing changes.
	r := a.call("PATCH", "/v1/wallets/me", map[string]any{"daily_limit": 100}, "Authorization", "Bearer ada-token")
	wantStatus(t, r, 403, "otp_invalid")
	r = a.call("PATCH", "/v1/wallets/me", map[string]any{"daily_limit": 100}, "Authorization", "Bearer ada-token", "X-OTP", "000000")
	wantStatus(t, r, 403, "otp_invalid")

	r = a.asAda("PATCH", "/v1/wallets/me", map[string]any{"daily_limit": 100})
	wantStatus(t, r, 200, "")
	if r.body["daily_limit"] != 100.0 {
		t.Errorf("wallet = %s", r.raw)
	}

	// An account limit within the wallet limit; one over it is refused.
	r = a.asAda("PATCH", "/v1/accounts/"+id, map[string]any{"daily_limit": 60.25})
	wantStatus(t, r, 200, "")
	r = a.asAda("POST", "/v1/accounts", map[string]any{"name": "b", "daily_limit": 40})
	wantStatus(t, r, 409, "limit_exceeds_wallet_limit")

	// null clears; absent leaves alone.
	r = a.asAda("PATCH", "/v1/accounts/"+id, map[string]any{"name": "renamed"})
	if r.body["daily_limit"] != 60.25 {
		t.Errorf("an absent limit must be left alone: %s", r.raw)
	}
	r = a.asAda("PATCH", "/v1/accounts/"+id, `{"daily_limit":null}`)
	wantStatus(t, r, 200, "")
	if r.body["daily_limit"] != nil {
		t.Errorf("limit = %v, want cleared", r.body["daily_limit"])
	}
}

func TestFreezeNeedsNoCodeButUnfreezeAndCloseDo(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 0)
	bearer := []string{"Authorization", "Bearer ada-token"}

	wantStatus(t, a.call("PATCH", "/v1/accounts/"+id, map[string]any{"status": "frozen"}, bearer...), 200, "")
	wantStatus(t, a.call("PATCH", "/v1/accounts/"+id, map[string]any{"status": "active"}, bearer...), 403, "otp_invalid")

	r := a.asAda("PATCH", "/v1/accounts/"+id, map[string]any{"status": "ACTIVE"})
	wantStatus(t, r, 200, "")
	if r.body["status"] != "active" {
		t.Errorf("status = %v", r.body["status"])
	}

	wantStatus(t, a.call("DELETE", "/v1/accounts/"+id, nil, bearer...), 403, "otp_invalid")
	r = a.asAda("DELETE", "/v1/accounts/"+id, nil)
	wantStatus(t, r, 200, "")
	if r.body["status"] != "closed" || r.body["closed_at"] == nil {
		t.Errorf("closed = %s", r.raw)
	}
}

func TestClosingAnAccountThatStillHasMoneyIsAConflict(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 500)

	r := a.asAda("DELETE", "/v1/accounts/"+id, nil)
	wantStatus(t, r, 409, "conflict")
	if !strings.Contains(r.body["message"].(string), "balance") {
		t.Errorf("message = %v", r.body["message"])
	}
}

func TestDepositAndWithdrawalNeedAKeyACodeAndFunding(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 0)
	path := "/v1/accounts/" + id + "/deposits"
	bearer := []string{"Authorization", "Bearer ada-token"}

	// No Idempotency-Key.
	wantStatus(t, a.asAda("POST", path, map[string]any{"amount": 5}), 400, "invalid_request")
	// No code.
	wantStatus(t, a.call("POST", path, map[string]any{"amount": 5}, append(bearer, "Idempotency-Key", testsupport.UUID(1))...), 403, "otp_invalid")

	r := a.money("ada", path, map[string]any{"amount": 25.5})
	wantStatus(t, r, 201, "")
	if r.body["status"] != "completed" || r.body["kind"] != "deposit" || r.body["amount"] != 25.5 || r.body["account_id"] != id {
		t.Errorf("deposit = %s", r.raw)
	}

	r = a.money("ada", "/v1/accounts/"+id+"/withdrawals", map[string]any{"amount": 10.25})
	wantStatus(t, r, 201, "")
	if r.body["kind"] != "withdrawal" || r.body["spending"] != true {
		t.Errorf("withdrawal = %s", r.raw)
	}
	if got := a.asAda("GET", "/v1/accounts/"+id, nil); got.body["balance"] != 15.25 {
		t.Errorf("balance = %v, want 15.25", got.body["balance"])
	}

	// Funding switched off: authenticated and code-checked, then refused.
	off := newAPI(t, func(o *application.Options) { o.FundingEnabled = false })
	offID := off.account("ada", "a", 100)
	wantStatus(t, off.money("ada", "/v1/accounts/"+offID+"/deposits", map[string]any{"amount": 5}), 403, "funding_disabled")
	wantStatus(t, off.money("ada", "/v1/accounts/"+offID+"/withdrawals", map[string]any{"amount": 5}), 403, "funding_disabled")
}

func TestInsufficientFundsIs422WithDecimalAmounts(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 1050)

	r := a.money("ada", "/v1/accounts/"+id+"/withdrawals", map[string]any{"amount": 20})
	wantStatus(t, r, 422, "insufficient_funds")
	if !strings.Contains(r.body["message"].(string), "10.50 available") || !strings.Contains(r.body["message"].(string), "20.00 requested") {
		t.Errorf("message = %v", r.body["message"])
	}
}

func TestTransfersMoveMoneyAndOtherPeoplesAccountsAreNotFound(t *testing.T) {
	a := newAPI(t)
	from := a.account("ada", "a", 1000)
	own := a.account("ada", "b", 0)
	bobs := a.account("bob", "c", 0)

	r := a.money("ada", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": own, "amount": 3.5})
	wantStatus(t, r, 201, "")
	if r.body["kind"] != "transfer" || r.body["counterparty_account_id"] != own || r.body["spending"] != false {
		t.Errorf("own transfer = %s", r.raw)
	}

	r = a.money("ada", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": bobs, "amount": 2})
	wantStatus(t, r, 201, "")
	if r.body["spending"] != true {
		t.Errorf("transfer to another user = %s", r.raw)
	}
	opID := r.body["id"].(string)

	// Bob sees it arrive, and cannot send from Ada's account.
	wantStatus(t, a.asBob("GET", "/v1/operations/"+opID, nil), 200, "")
	wantStatus(t, a.money("bob", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": bobs, "amount": 1}), 404, "not_found")
	wantStatus(t, a.money("ada", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": testsupport.UUID(404), "amount": 1}), 404, "not_found")
	wantStatus(t, a.asBob("GET", "/v1/accounts/"+from, nil), 404, "not_found")
}

func TestARetriedRequestReturnsTheSameOperationAndAKeyCannotBeReused(t *testing.T) {
	a := newAPI(t)
	from := a.account("ada", "a", 100_000) // 1000.00
	to := a.account("bob", "b", 0)
	headers := []string{"Idempotency-Key", testsupport.UUID(4242)}
	body := map[string]any{"from_account_id": from, "to_account_id": to, "amount": 10}

	first := a.asAda("POST", "/v1/transfers", body, headers...)
	again := a.asAda("POST", "/v1/transfers", body, headers...)
	wantStatus(t, first, 201, "")
	wantStatus(t, again, 201, "")
	if first.body["id"] != again.body["id"] {
		t.Errorf("the retry made a second operation: %v vs %v", first.body["id"], again.body["id"])
	}
	if got := a.Ledger.BalanceOf(from); got != 99_000 {
		t.Errorf("balance = %d, want 99000 (10.00 moved once)", got)
	}

	body["amount"] = 11
	wantStatus(t, a.asAda("POST", "/v1/transfers", body, headers...), 409, "idempotency_key_reused")
}

func TestAnOperationStuckOnTheLedgerIs202AndFinishesLater(t *testing.T) {
	a := newAPI(t)
	from := a.account("ada", "a", 1000)
	to := a.account("bob", "b", 0)

	a.Ledger.Fault = func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == "Debit" {
			return testsupport.FaultUnavailable
		}

		return testsupport.FaultNone
	}
	r := a.money("ada", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": to, "amount": 10})
	wantStatus(t, r, 202, "")
	if r.body["status"] != "pending" {
		t.Errorf("body = %s", r.raw)
	}
	id := r.body["id"].(string)

	wantStatus(t, a.asAda("GET", "/v1/operations/"+id, nil), 200, "")
	if got := a.asAda("GET", "/v1/operations/"+id, nil); got.body["status"] != "pending" {
		t.Errorf("polled = %s", got.raw)
	}

	a.Ledger.Fault = nil
	a.Clock.Advance(time.Minute)
	if _, err := a.Svc.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := a.asAda("GET", "/v1/operations/"+id, nil); got.body["status"] != "completed" {
		t.Errorf("after recovery = %s", got.raw)
	}
}

func TestALedgerThatIsDownBeforeAnythingIsRecordedIs503(t *testing.T) {
	a := newAPI(t)
	from := a.account("ada", "a", 1000)
	to := a.account("bob", "b", 0)

	a.Ledger.Fault = func(testsupport.LedgerCall) testsupport.Fault { return testsupport.FaultUnavailable }
	wantStatus(t, a.money("ada", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": to, "amount": 10}), 503, "unavailable")
	wantStatus(t, a.asAda("GET", "/v1/accounts/"+from, nil), 503, "unavailable")
}

func TestAnOperationTheLedgerRefusesIs422WithTheOperation(t *testing.T) {
	a := newAPI(t)
	from := a.account("ada", "a", 1000)
	to := a.account("bob", "b", 0)

	a.Ledger.Fault = func(c testsupport.LedgerCall) testsupport.Fault {
		if c.Method == "Debit" {
			return testsupport.FaultRejected
		}

		return testsupport.FaultNone
	}
	r := a.money("ada", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": to, "amount": 10})
	wantStatus(t, r, 422, "operation_failed")

	op, _ := r.body["operation"].(map[string]any)
	if op["status"] != "failed" || op["failure_reason"] != "rejected" {
		t.Errorf("operation = %v", op)
	}
}

func TestGoingOverALimitNeedsTwoStepsToConfirm(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 100_000) // 1000.00
	wantStatus(t, a.asAda("PATCH", "/v1/accounts/"+id, map[string]any{"daily_limit": 100}), 200, "")

	r := a.money("ada", "/v1/accounts/"+id+"/withdrawals", map[string]any{"amount": 150})
	wantStatus(t, r, 409, "confirmation_required")
	op, _ := r.body["operation"].(map[string]any)
	if op["status"] != "awaiting_confirmation" || op["id"] == nil {
		t.Fatalf("operation = %v", op)
	}
	confirmation, _ := op["confirmation"].(map[string]any)
	if confirmation["limit_scope"] != "account" || confirmation["expires_at"] == nil {
		t.Errorf("confirmation = %v", confirmation)
	}
	opID := op["id"].(string)

	// The state is visible by polling, and nothing has moved.
	if got := a.asAda("GET", "/v1/operations/"+opID, nil); got.body["status"] != "awaiting_confirmation" {
		t.Errorf("polled = %s", got.raw)
	}
	if got := a.Ledger.BalanceOf(id); got != 100_000 {
		t.Errorf("balance = %d: nothing may move before the confirmation", got)
	}

	// Step two needs the explicit acknowledgement and a code.
	wantStatus(t, a.asAda("POST", "/v1/operations/"+opID+"/confirm", map[string]any{}), 400, "invalid_request")
	wantStatus(t, a.asAda("POST", "/v1/operations/"+opID+"/confirm", map[string]any{"acknowledge_over_limit": false}), 400, "invalid_request")
	wantStatus(t, a.call("POST", "/v1/operations/"+opID+"/confirm", map[string]any{"acknowledge_over_limit": true}, "Authorization", "Bearer ada-token"), 403, "otp_invalid")
	wantStatus(t, a.asBob("POST", "/v1/operations/"+opID+"/confirm", map[string]any{"acknowledge_over_limit": true}), 404, "not_found")

	done := a.asAda("POST", "/v1/operations/"+opID+"/confirm", map[string]any{"acknowledge_over_limit": true})
	wantStatus(t, done, 201, "")
	if done.body["status"] != "completed" {
		t.Errorf("confirmed = %s", done.raw)
	}
	if got := a.Ledger.BalanceOf(id); got != 85_000 {
		t.Errorf("balance = %d, want 85000 (150.00 spent)", got)
	}
}

func TestAnAwaitingOperationCanBeCancelledOrTimesOut(t *testing.T) {
	a := newAPI(t)
	id := a.account("ada", "a", 100_000)
	wantStatus(t, a.asAda("PATCH", "/v1/accounts/"+id, map[string]any{"daily_limit": 100}), 200, "")

	r := a.money("ada", "/v1/accounts/"+id+"/withdrawals", map[string]any{"amount": 150})
	opID := r.body["operation"].(map[string]any)["id"].(string)

	cancelled := a.call("POST", "/v1/operations/"+opID+"/cancel", nil, "Authorization", "Bearer ada-token") // no code
	wantStatus(t, cancelled, 200, "")
	if cancelled.body["status"] != "failed" || cancelled.body["failure_reason"] != "cancelled" {
		t.Errorf("cancelled = %s", cancelled.raw)
	}
	wantStatus(t, a.asAda("POST", "/v1/operations/"+opID+"/confirm", map[string]any{"acknowledge_over_limit": true}), 409, "conflict")

	r = a.money("ada", "/v1/accounts/"+id+"/withdrawals", map[string]any{"amount": 150})
	opID = r.body["operation"].(map[string]any)["id"].(string)
	a.Clock.Advance(11 * time.Minute)
	expired := a.asAda("POST", "/v1/operations/"+opID+"/confirm", map[string]any{"acknowledge_over_limit": true})
	wantStatus(t, expired, 409, "conflict")
	if got := a.asAda("GET", "/v1/operations/"+opID, nil); got.body["failure_reason"] != "confirmation_expired" {
		t.Errorf("after the window = %s", got.raw)
	}
}

func TestTransactionsArePagedWithACursor(t *testing.T) {
	a := newAPI(t)
	from := a.account("ada", "a", 10_000)
	to := a.account("bob", "b", 0)

	var want []string
	for i := 0; i < 5; i++ {
		a.Clock.Advance(time.Second)
		r := a.money("ada", "/v1/transfers", map[string]any{"from_account_id": from, "to_account_id": to, "amount": 1})
		wantStatus(t, r, 201, "")
		want = append([]string{r.body["id"].(string)}, want...)
	}

	var got []string
	path := "/v1/accounts/" + from + "/transactions?limit=2"
	for pages := 0; pages < 5; pages++ {
		r := a.asAda("GET", path, nil)
		wantStatus(t, r, 200, "")

		items, _ := r.body["transactions"].([]any)
		for _, item := range items {
			got = append(got, item.(map[string]any)["id"].(string))
		}

		next, _ := r.body["next_cursor"].(string)
		if next == "" {
			break
		}
		path = "/v1/accounts/" + from + "/transactions?limit=2&cursor=" + next
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("transactions = %v, want %v", got, want)
	}

	wantStatus(t, a.asAda("GET", "/v1/accounts/"+from+"/transactions?cursor=garbage", nil), 400, "invalid_request")
	wantStatus(t, a.asAda("GET", "/v1/accounts/"+from+"/transactions?limit=0", nil), 400, "invalid_request")
	wantStatus(t, a.asAda("GET", "/v1/accounts/"+from+"/transactions?limit=abc", nil), 400, "invalid_request")
	wantStatus(t, a.asBob("GET", "/v1/accounts/"+from+"/transactions", nil), 404, "not_found")

	// A short page has no next cursor.
	r := a.asAda("GET", "/v1/accounts/"+from+"/transactions", nil)
	if _, has := r.body["next_cursor"]; has {
		t.Errorf("a page that is not full should not offer a cursor: %s", r.raw)
	}
}

func TestResponsesAreNeverCached(t *testing.T) {
	a := newAPI(t)

	r := a.asAda("GET", "/v1/wallets/me", nil)
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := r.header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
}
