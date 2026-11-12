// Package httpapi is the driving adapter that exposes the wallet over REST, for users, through
// the API gateway. It translates JSON and headers into calls on application.Wallets and the
// answers back; it holds no rules of its own.
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

const (
	maxBodyBytes = 64 << 10

	headerOTP            = "X-OTP"
	headerIdempotencyKey = "Idempotency-Key"
)

// Server serves the REST API.
type Server struct {
	wallets application.Wallets
	log     *slog.Logger
	mux     *http.ServeMux
}

// New returns the REST handler. It is the caller's job to run it and to put it behind the gateway;
// who the caller is comes from the bearer token, checked through the core, never from a header
// the gateway sets.
func New(wallets application.Wallets, log *slog.Logger) http.Handler {
	s := &Server{wallets: wallets, log: log, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /health", s.health)

	s.route("GET /v1/wallets/me", s.getWallet)
	s.route("PATCH /v1/wallets/me", s.patchWallet)
	s.route("POST /v1/accounts", s.createAccount)
	s.route("GET /v1/accounts/{id}", s.getAccount)
	s.route("PATCH /v1/accounts/{id}", s.patchAccount)
	s.route("DELETE /v1/accounts/{id}", s.closeAccount)
	s.route("GET /v1/accounts/{id}/transactions", s.listTransactions)
	s.route("POST /v1/accounts/{id}/deposits", s.deposit)
	s.route("POST /v1/accounts/{id}/withdrawals", s.withdraw)
	s.route("POST /v1/transfers", s.transfer)
	s.route("GET /v1/operations/{id}", s.getOperation)
	s.route("POST /v1/operations/{id}/confirm", s.confirm)
	s.route("POST /v1/operations/{id}/cancel", s.cancel)

	return s.logging(s.jsonErrors(s.mux))
}

type userKey struct{}

// authed is a handler that knows who is calling.
type authed func(w http.ResponseWriter, r *http.Request, userID string)

// route registers an authenticated handler. The bearer token is checked with the auth service on
// every request; X-User-ID, which the gateway also sets, is deliberately not trusted.
func (s *Server) route(pattern string, handle authed) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		userID, err := s.wallets.Authenticate(r.Context(), bearerToken(r))
		if err != nil {
			s.fail(w, r, err)

			return
		}

		handle(w, r.WithContext(context.WithValue(r.Context(), userKey{}, userID)), userID)
	})
}

func bearerToken(r *http.Request) string {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}

	return strings.TrimSpace(token)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) getWallet(w http.ResponseWriter, r *http.Request, user string) {
	wallet, err := s.wallets.Wallet(r.Context(), user)
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, walletOut(wallet))
}

func (s *Server) patchWallet(w http.ResponseWriter, r *http.Request, user string) {
	var body walletPatchBody
	if !s.decode(w, r, &body) {
		return
	}

	change, err := body.DailyLimit.change()
	if err != nil {
		s.fail(w, r, err)

		return
	}

	wallet, err := s.wallets.SetWalletLimit(r.Context(), user, r.Header.Get(headerOTP), change)
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, walletOut(wallet))
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request, user string) {
	var body createAccountBody
	if !s.decode(w, r, &body) {
		return
	}

	var limit *int64
	if body.DailyLimit != nil {
		change, err := body.DailyLimit.change()
		if err != nil {
			s.fail(w, r, err)

			return
		}
		limit = change.Value
	}

	account, err := s.wallets.CreateAccount(r.Context(), user, body.Name, limit)
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusCreated, accountOut(account))
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request, user string) {
	account, err := s.wallets.Account(r.Context(), user, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, accountOut(account))
}

func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request, user string) {
	var body patchAccountBody
	if !s.decode(w, r, &body) {
		return
	}

	change, err := body.DailyLimit.change()
	if err != nil {
		s.fail(w, r, err)

		return
	}

	patch := application.AccountPatch{Name: body.Name, Limit: change}
	if body.Status != nil {
		switch strings.ToLower(*body.Status) {
		case "active":
			status := domain.AccountActive
			patch.Status = &status
		case "frozen":
			status := domain.AccountFrozen
			patch.Status = &status
		default:
			s.fail(w, r, domain.Invalidf(`status must be "active" or "frozen"`))

			return
		}
	}

	account, err := s.wallets.UpdateAccount(r.Context(), user, r.PathValue("id"), r.Header.Get(headerOTP), patch)
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, accountOut(account))
}

func (s *Server) closeAccount(w http.ResponseWriter, r *http.Request, user string) {
	account, err := s.wallets.CloseAccount(r.Context(), user, r.PathValue("id"), r.Header.Get(headerOTP))
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, accountOut(account))
}

func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request, user string) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.fail(w, r, domain.Invalidf("limit must be a positive whole number"))

			return
		}
		limit = n
	}

	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		s.fail(w, r, err)

		return
	}

	ops, err := s.wallets.Transactions(r.Context(), user, r.PathValue("id"), cursor, limit)
	if err != nil {
		s.fail(w, r, err)

		return
	}

	body := struct {
		Transactions []operationJSON `json:"transactions"`
		NextCursor   string          `json:"next_cursor,omitempty"`
	}{Transactions: make([]operationJSON, 0, len(ops))}
	for _, op := range ops {
		body.Transactions = append(body.Transactions, operationOut(op))
	}
	// A full page may have more behind it; a short one is the end.
	if size := pageSize(limit); len(ops) == size {
		body.NextCursor = encodeCursor(ops[len(ops)-1])
	}

	writeJSON(w, http.StatusOK, body)
}

// pageSize mirrors the core's default and cap, to know when a page is full.
func pageSize(limit int) int {
	switch {
	case limit <= 0:
		return 50
	case limit > 200:
		return 200
	default:
		return limit
	}
}

func encodeCursor(op domain.Operation) string {
	raw := fmt.Sprintf("%d:%s", op.CreatedAt.UnixMicro(), op.ID)

	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(raw string) (*application.OperationCursor, error) {
	if raw == "" {
		return nil, nil
	}

	bad := domain.Invalidf("cursor is not valid")

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, bad
	}
	micros, id, found := strings.Cut(string(decoded), ":")
	if !found {
		return nil, bad
	}
	n, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return nil, bad
	}
	if id, err = domain.ParseID("cursor", id); err != nil {
		return nil, bad
	}

	return &application.OperationCursor{CreatedAt: time.UnixMicro(n).UTC(), ID: id}, nil
}

func (s *Server) deposit(w http.ResponseWriter, r *http.Request, user string) {
	s.userMoney(w, r, user, s.wallets.Deposit)
}

func (s *Server) withdraw(w http.ResponseWriter, r *http.Request, user string) {
	s.userMoney(w, r, user, s.wallets.Withdraw)
}

func (s *Server) userMoney(w http.ResponseWriter, r *http.Request, user string, do func(context.Context, application.UserMoneyRequest) (domain.Operation, error)) {
	var body amountBody
	if !s.decode(w, r, &body) {
		return
	}
	cents, err := amountIn(body.Amount)
	if err != nil {
		s.fail(w, r, err)

		return
	}

	op, err := do(r.Context(), application.UserMoneyRequest{
		UserID: user, AccountID: r.PathValue("id"), Amount: cents,
		IdempotencyKey: r.Header.Get(headerIdempotencyKey), OTP: r.Header.Get(headerOTP),
	})
	s.respondOperation(w, r, op, err)
}

func (s *Server) transfer(w http.ResponseWriter, r *http.Request, user string) {
	var body transferBody
	if !s.decode(w, r, &body) {
		return
	}
	cents, err := amountIn(body.Amount)
	if err != nil {
		s.fail(w, r, err)

		return
	}

	op, err := s.wallets.Transfer(r.Context(), application.TransferRequest{
		UserID: user, FromAccountID: body.FromAccountID, ToAccountID: body.ToAccountID, Amount: cents,
		IdempotencyKey: r.Header.Get(headerIdempotencyKey), OTP: r.Header.Get(headerOTP),
	})
	s.respondOperation(w, r, op, err)
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request, user string) {
	op, err := s.wallets.Operation(r.Context(), user, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, operationOut(op))
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request, user string) {
	var body confirmBody
	if !s.decode(w, r, &body) {
		return
	}

	op, err := s.wallets.Confirm(r.Context(), application.ConfirmRequest{
		UserID: user, OperationID: r.PathValue("id"), OTP: r.Header.Get(headerOTP), Acknowledged: body.AcknowledgeOverLimit,
	})
	s.respondOperation(w, r, op, err)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, user string) {
	op, err := s.wallets.Cancel(r.Context(), user, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, operationOut(op))
}

// respondOperation answers for an operation however far it got: 201 when it is done, 202 when it
// is still on its way (the client polls GET /v1/operations/{id}), 422 when it failed, and 409 with
// the operation when it is waiting for the user to confirm going over a limit.
func (s *Server) respondOperation(w http.ResponseWriter, r *http.Request, op domain.Operation, err error) {
	var confirmation *domain.ConfirmationRequiredError

	switch {
	case errors.As(err, &confirmation):
		status, body, _ := classify(err)
		out := operationOut(op)
		body.Operation = &out
		writeJSON(w, status, body)
	case err != nil:
		s.fail(w, r, err)
	case op.Status == domain.StatusCompleted:
		writeJSON(w, http.StatusCreated, operationOut(op))
	case op.Status == domain.StatusFailed:
		out := operationOut(op)
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{
			Message: "the operation failed: " + op.Failure.String(), Code: "operation_failed", Operation: &out,
		})
	default:
		writeJSON(w, http.StatusAccepted, operationOut(op))
	}
}

// amountIn casts a JSON amount to cents. A missing amount is an error, not zero.
func amountIn(amount *float64) (int64, error) {
	if amount == nil {
		return 0, domain.Invalidf("amount is required")
	}

	return processmoney.ToMinorUnits(*amount)
}

// decode reads a JSON body into v. Unknown fields, trailing data and oversized bodies are
// rejected: a typo in a field name must not silently turn into a default.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	err := decoder.Decode(v)
	if errors.Is(err, io.EOF) {
		// No body at all: fine for an endpoint whose fields are all optional.
		return true
	}
	if err == nil && decoder.More() {
		err = errors.New("unexpected data after the JSON body")
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Message: "request body is too large", Code: "body_too_large"})

			return false
		}

		s.fail(w, r, domain.Invalidf("the request body is not valid JSON for this endpoint: %v", err))

		return false
	}

	return true
}

// fail writes an error response, logging the ones that are the server's fault.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, body, unexpected := classify(err)
	if unexpected {
		s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}

	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusRecorder remembers the status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logging writes one line per request. It logs the method, the path and the outcome, and nothing
// of the headers or the body, which carry tokens and one-time codes.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if v := recover(); v != nil {
				s.log.ErrorContext(r.Context(), "handler panicked", "method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(v))
				if rec.status == http.StatusOK {
					writeJSON(rec, http.StatusInternalServerError, errorBody{Message: "internal error", Code: "internal_error"})
				}
			}

			s.log.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		}()

		next.ServeHTTP(rec, r)
	})
}

// jsonErrors turns the router's own plain-text 404 and 405 into the same JSON error as everything
// else.
func (s *Server) jsonErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&errorRewriter{ResponseWriter: w}, r)
	})
}

type errorRewriter struct {
	http.ResponseWriter
	swallow bool
}

func (e *errorRewriter) WriteHeader(code int) {
	isText := strings.HasPrefix(e.Header().Get("Content-Type"), "text/plain")

	if isText && (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) {
		body := errorBody{Message: "not found", Code: "not_found"}
		if code == http.StatusMethodNotAllowed {
			body = errorBody{Message: "method not allowed", Code: "method_not_allowed"}
		}

		e.Header().Del("Content-Length")
		writeJSON(e.ResponseWriter, code, body)
		e.swallow = true

		return
	}

	e.ResponseWriter.WriteHeader(code)
}

func (e *errorRewriter) Write(b []byte) (int, error) {
	if e.swallow {
		return len(b), nil
	}

	return e.ResponseWriter.Write(b)
}
