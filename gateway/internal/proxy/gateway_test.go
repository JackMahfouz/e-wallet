package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/gateway/internal/authfilter"
	"github.com/JackMahfouz/e-wallet/gateway/internal/config"
	"github.com/JackMahfouz/e-wallet/gateway/internal/proxy"
	"github.com/JackMahfouz/e-wallet/gateway/internal/ratelimit"
)

const configYAML = `
server:
  max_body_bytes: 64
upstreams:
  identity: {url: "%s", path_prefix: /api, timeout: 300ms}
routes:
  - {name: public, methods: [POST], path: /v1/auth/login, upstream: identity, rate_limit: {requests: 2, window: 1m}}
  - {name: open, methods: [GET, POST], path: /v1/open, upstream: identity}
  - {name: protected, methods: [GET], path: /v1/auth/me, upstream: identity, auth: true}
  - {name: limited-protected, methods: [GET], path: /v1/limited-me, upstream: identity, auth: true, rate_limit: {requests: 5, window: 30s}}
  - {name: tree, methods: [GET], path: /v1/tree/, upstream: identity}
  - {name: tree-deep, methods: [GET], path: /v1/tree/deep/, upstream: identity, auth: true}
`

// stubUpstream is a fake backend that records what the gateway sends it.
type stubUpstream struct {
	server *httptest.Server

	mu      sync.Mutex
	calls   int
	method  string
	uri     string
	header  http.Header
	body    string
	handler http.HandlerFunc
}

func newStubUpstream(t *testing.T, handler http.HandlerFunc) *stubUpstream {
	t.Helper()

	stub := &stubUpstream{handler: handler}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stub.mu.Lock()
		stub.calls++
		stub.method, stub.uri, stub.header, stub.body = r.Method, r.URL.RequestURI(), r.Header.Clone(), string(body)
		stub.mu.Unlock()

		if stub.handler != nil {
			stub.handler(w, r)

			return
		}
		w.Header().Set("X-Upstream", "yes")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"upstream"}`))
	}))
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *stubUpstream) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

type fakeVerifier struct {
	mu      sync.Mutex
	tokens  []string
	subject string
	err     error
}

func (f *fakeVerifier) Verify(_ context.Context, token string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)

	return f.subject, f.err
}

func (f *fakeVerifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.tokens)
}

type limiterCall struct {
	key    string
	limit  int
	window time.Duration
}

type fakeLimiter struct {
	mu     sync.Mutex
	calls  []limiterCall
	result ratelimit.Result
	err    error
}

func (f *fakeLimiter) Allow(_ context.Context, key string, limit int, window time.Duration) (ratelimit.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, limiterCall{key, limit, window})

	return f.result, f.err
}

func (f *fakeLimiter) callList() []limiterCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]limiterCall(nil), f.calls...)
}

type harness struct {
	gateway  *proxy.Gateway
	upstream *stubUpstream
	verifier *fakeVerifier
	limiter  *fakeLimiter
	logs     *bytes.Buffer
}

// newHarness wires a gateway to a stub upstream, a verifier that accepts every token as user 42,
// and a limiter that allows everything. Tests change those fakes to provoke each behaviour.
func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()

	return newHarnessFor(t, newStubUpstream(t, nil), mutate...)
}

func newHarnessFor(t *testing.T, upstream *stubUpstream, mutate ...func(*config.Config)) *harness {
	t.Helper()

	cfg, err := config.Parse(fmt.Appendf(nil, configYAML, upstream.server.URL))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	for _, change := range mutate {
		change(cfg)
	}

	h := &harness{
		upstream: upstream,
		verifier: &fakeVerifier{subject: "42"},
		limiter:  &fakeLimiter{result: ratelimit.Result{Allowed: true}},
		logs:     &bytes.Buffer{},
	}
	h.gateway, err = proxy.New(proxy.Options{
		Config:   cfg,
		Verifier: h.verifier,
		Limiter:  h.limiter,
		Logger:   slog.New(slog.NewTextHandler(h.logs, nil)),
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	return h
}

func (h *harness) do(method, target string, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)

	return rec
}

func assertJSONMessage(t *testing.T, rec *httptest.ResponseRecorder, status int, message string) {
	t.Helper()

	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != fmt.Sprintf(`{"message":%q}`, message) {
		t.Errorf("body = %s, want {\"message\":%q}", got, message)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestProxiesToTheUpstreamWithPrefixMethodQueryAndBody(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodPost, "/v1/auth/login?remember=1&x=a%20b", `{"email":"a@b.c"}`, "Content-Type", "application/json")

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"from":"upstream"}` {
		t.Errorf("response = %d %s, want the upstream's 200 body", rec.Code, rec.Body)
	}
	if h.upstream.method != http.MethodPost {
		t.Errorf("upstream method = %s, want POST", h.upstream.method)
	}
	if want := "/api/v1/auth/login?remember=1&x=a%20b"; h.upstream.uri != want {
		t.Errorf("upstream URI = %s, want %s (prefix added, query kept)", h.upstream.uri, want)
	}
	if h.upstream.body != `{"email":"a@b.c"}` {
		t.Errorf("upstream body = %q", h.upstream.body)
	}
	if got := h.upstream.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("upstream Content-Type = %q, want it forwarded", got)
	}
}

func TestPassesUpstreamResponsesThroughUnchanged(t *testing.T) {
	upstream := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom", "kept")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"The email field is required."}`))
	})
	h := newHarnessFor(t, upstream)

	rec := h.do(http.MethodPost, "/v1/auth/login", `{}`)

	if rec.Code != http.StatusUnprocessableEntity || rec.Body.String() != `{"message":"The email field is required."}` {
		t.Errorf("response = %d %s, want the upstream's 422 untouched", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Custom"); got != "kept" {
		t.Errorf("X-Custom = %q, want upstream headers passed through", got)
	}
}

func TestProtectedRouteRefusesRequestsWithoutAUsableBearerToken(t *testing.T) {
	tests := map[string][]string{
		"no authorization header": nil,
		"basic auth":              {"Authorization", "Basic dXNlcjpwYXNz"},
		"bearer without a token":  {"Authorization", "Bearer "},
		"token without a scheme":  {"Authorization", "abc.def.ghi"},
	}

	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)

			rec := h.do(http.MethodGet, "/v1/auth/me", "", headers...)

			assertJSONMessage(t, rec, http.StatusUnauthorized, "Unauthenticated.")
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if h.verifier.callCount() != 0 || h.upstream.callCount() != 0 {
				t.Errorf("verifier called %d times, upstream %d; want neither", h.verifier.callCount(), h.upstream.callCount())
			}
		})
	}
}

func TestProtectedRouteRefusesATokenTheAuthServiceRejects(t *testing.T) {
	h := newHarness(t)
	h.verifier.err = authfilter.ErrUnauthenticated

	rec := h.do(http.MethodGet, "/v1/auth/me", "", "Authorization", "Bearer forged.jwt.token")

	assertJSONMessage(t, rec, http.StatusUnauthorized, "Unauthenticated.")
	if h.upstream.callCount() != 0 {
		t.Error("the upstream was called for a rejected token")
	}
}

func TestProtectedRouteForwardsTheVerifiedUserAndTheOriginalAuthorizationHeader(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodGet, "/v1/auth/me", "", "Authorization", "bearer good.jwt.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if got := h.upstream.header.Get("X-User-ID"); got != "42" {
		t.Errorf("upstream X-User-ID = %q, want 42", got)
	}
	if got := h.upstream.header.Get("Authorization"); got != "bearer good.jwt.token" {
		t.Errorf("upstream Authorization = %q, want it forwarded unchanged so Identity can re-verify", got)
	}
	if h.verifier.tokens[0] != "good.jwt.token" {
		t.Errorf("verified token = %q, want the token without its scheme (and a case-insensitive scheme accepted)", h.verifier.tokens[0])
	}
}

func TestProtectedRouteFailsClosedWhenTheAuthServiceIsUnavailable(t *testing.T) {
	h := newHarness(t)
	h.verifier.err = errors.New("auth service down")

	rec := h.do(http.MethodGet, "/v1/auth/me", "", "Authorization", "Bearer good.jwt.token")

	assertJSONMessage(t, rec, http.StatusServiceUnavailable, "Authentication service unavailable.")
	if h.upstream.callCount() != 0 {
		t.Error("the upstream was called although the token could not be verified")
	}
}

func TestStripsAClientSuppliedUserIDOnPublicRoutes(t *testing.T) {
	h := newHarness(t)

	h.do(http.MethodPost, "/v1/auth/login", `{}`, "X-User-ID", "999")

	if values := h.upstream.header.Values("X-User-ID"); len(values) != 0 {
		t.Errorf("upstream saw X-User-ID %v, want none: clients must not be able to claim an identity", values)
	}
}

func TestReplacesAClientSuppliedUserIDOnProtectedRoutes(t *testing.T) {
	h := newHarness(t)

	h.do(http.MethodGet, "/v1/auth/me", "", "Authorization", "Bearer good.jwt.token", "X-User-ID", "999")

	if values := h.upstream.header.Values("X-User-ID"); len(values) != 1 || values[0] != "42" {
		t.Errorf("upstream saw X-User-ID %v, want exactly [42]", values)
	}
}

func TestDropsUnderscoreHeadersThatCouldImpersonateTrustedOnes(t *testing.T) {
	// PHP folds X_User_ID and X-User-ID into one variable, so an upstream could not tell a
	// client's X_User_ID from the gateway's X-User-ID.
	routes := map[string][]string{
		"public route":    {http.MethodPost, "/v1/auth/login"},
		"protected route": {http.MethodGet, "/v1/auth/me"},
	}

	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)

			h.do(route[0], route[1], `{}`,
				"Authorization", "Bearer good.jwt.token",
				"X_User_ID", "999",
				"X_Forwarded_For", "6.6.6.6",
				"X-Request-Source", "mobile")

			for header := range h.upstream.header {
				if strings.Contains(header, "_") {
					t.Errorf("upstream received the underscore header %q", header)
				}
			}
			if got := h.upstream.header.Get("X-Request-Source"); got != "mobile" {
				t.Errorf("ordinary header X-Request-Source = %q, want it forwarded untouched", got)
			}
			if values := h.upstream.header.Values("X-User-ID"); route[0] == http.MethodGet && (len(values) != 1 || values[0] != "42") {
				t.Errorf("X-User-ID = %v, want exactly [42] on the protected route", values)
			}
		})
	}
}

func TestReturnsTooManyRequestsWithRetryAfterWithoutCallingTheUpstream(t *testing.T) {
	tests := map[string]struct {
		retryAfter time.Duration
		header     string
	}{
		"rounds up":           {29200 * time.Millisecond, "30"},
		"exact seconds":       {45 * time.Second, "45"},
		"at least one second": {200 * time.Millisecond, "1"},
		"zero is still 1":     {0, "1"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.limiter.result = ratelimit.Result{Allowed: false, RetryAfter: tc.retryAfter}

			rec := h.do(http.MethodPost, "/v1/auth/login", `{}`)

			assertJSONMessage(t, rec, http.StatusTooManyRequests, "Too many requests.")
			if got := rec.Header().Get("Retry-After"); got != tc.header {
				t.Errorf("Retry-After = %q, want %q", got, tc.header)
			}
			if h.upstream.callCount() != 0 {
				t.Error("the upstream was called for a rate-limited request")
			}
		})
	}
}

func TestRateLimitsPerRouteAndClientWithTheConfiguredLimit(t *testing.T) {
	h := newHarness(t)

	h.do(http.MethodPost, "/v1/auth/login", `{}`)

	calls := h.limiter.callList()
	if len(calls) != 1 {
		t.Fatalf("limiter called %d times, want 1", len(calls))
	}
	if want := (limiterCall{key: "public:192.0.2.1", limit: 2, window: time.Minute}); calls[0] != want {
		t.Errorf("limiter call = %+v, want %+v", calls[0], want)
	}
}

func TestRoutesWithoutARateLimitNeverTouchTheLimiter(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodGet, "/v1/open", "")

	if rec.Code != http.StatusOK || len(h.limiter.callList()) != 0 {
		t.Errorf("status = %d, limiter calls = %d; want 200 and no calls", rec.Code, len(h.limiter.callList()))
	}
}

func TestRateLimitIsCheckedBeforeTheTokenSoFloodsNeverReachTheAuthService(t *testing.T) {
	h := newHarness(t)
	h.limiter.result = ratelimit.Result{Allowed: false, RetryAfter: time.Second}

	rec := h.do(http.MethodGet, "/v1/limited-me", "", "Authorization", "Bearer any.jwt.token")

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if h.verifier.callCount() != 0 {
		t.Errorf("verifier called %d times, want 0", h.verifier.callCount())
	}
}

func TestServesTheRequestWhenTheLimiterItselfFails(t *testing.T) {
	h := newHarness(t)
	h.limiter.err = errors.New("limiter exploded")

	rec := h.do(http.MethodPost, "/v1/auth/login", `{}`)

	if rec.Code != http.StatusOK || h.upstream.callCount() != 1 {
		t.Errorf("status = %d, upstream calls = %d; want the request served", rec.Code, h.upstream.callCount())
	}
	if !strings.Contains(h.logs.String(), "limiter exploded") {
		t.Errorf("limiter failure not logged:\n%s", h.logs)
	}
}

func TestClientAddressForRateLimitsAndForwardedHeaders(t *testing.T) {
	trusted := func(cfg *config.Config) { cfg.Server.ClientIPHeader = "X-Forwarded-For" }

	tests := map[string]struct {
		mutate        []func(*config.Config)
		xff           string
		wantKey       string
		wantForwarded string
	}{
		"direct clients: a spoofed header is ignored": {
			xff:           "6.6.6.6",
			wantKey:       "public:192.0.2.1",
			wantForwarded: "192.0.2.1",
		},
		"behind a trusted proxy: the last entry is the client": {
			mutate:        []func(*config.Config){trusted},
			xff:           "6.6.6.6, 203.0.113.7",
			wantKey:       "public:203.0.113.7",
			wantForwarded: "6.6.6.6, 203.0.113.7, 192.0.2.1",
		},
		"behind a trusted proxy: ipv6 is normalized": {
			mutate:  []func(*config.Config){trusted},
			xff:     "2001:DB8:0:0:0:0:0:1",
			wantKey: "public:2001:db8::1",
		},
		"behind a trusted proxy: junk falls back to the socket address": {
			mutate:  []func(*config.Config){trusted},
			xff:     "not-an-ip",
			wantKey: "public:192.0.2.1",
		},
		"behind a trusted proxy: no header falls back to the socket address": {
			mutate:  []func(*config.Config){trusted},
			wantKey: "public:192.0.2.1",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.mutate...)
			var headers []string
			if tc.xff != "" {
				headers = []string{"X-Forwarded-For", tc.xff}
			}

			h.do(http.MethodPost, "/v1/auth/login", `{}`, headers...)

			calls := h.limiter.callList()
			if len(calls) != 1 || calls[0].key != tc.wantKey {
				t.Errorf("limiter calls = %+v, want key %q", calls, tc.wantKey)
			}
			if tc.wantForwarded != "" {
				if got := h.upstream.header.Get("X-Forwarded-For"); got != tc.wantForwarded {
					t.Errorf("upstream X-Forwarded-For = %q, want %q", got, tc.wantForwarded)
				}
			}
		})
	}
}

func TestMatchesRoutesByPathAndMethod(t *testing.T) {
	h := newHarness(t)

	tests := map[string]struct {
		method, path string
		status       int
		allow        string
	}{
		"exact route":                        {http.MethodPost, "/v1/auth/login", http.StatusOK, ""},
		"second method of a route":           {http.MethodPost, "/v1/open", http.StatusOK, ""},
		"prefix route root":                  {http.MethodGet, "/v1/tree/", http.StatusOK, ""},
		"prefix route below the root":        {http.MethodGet, "/v1/tree/a/b", http.StatusOK, ""},
		"longest prefix wins and needs auth": {http.MethodGet, "/v1/tree/deep/x", http.StatusUnauthorized, ""},
		"unknown path":                       {http.MethodGet, "/v1/nope", http.StatusNotFound, ""},
		"exact path is not a prefix":         {http.MethodPost, "/v1/auth/login/extra", http.StatusNotFound, ""},
		"wrong method":                       {http.MethodGet, "/v1/auth/login", http.StatusMethodNotAllowed, "POST"},
		"head is not implied by get":         {http.MethodHead, "/v1/auth/me", http.StatusMethodNotAllowed, "GET"},
		"allow lists every method":           {http.MethodDelete, "/v1/open", http.StatusMethodNotAllowed, "GET, POST"},
		"wrong method on a prefix":           {http.MethodPost, "/v1/tree/a", http.StatusMethodNotAllowed, "GET"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := h.do(tc.method, tc.path, "")

			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body)
			}
			if got := rec.Header().Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q", got, tc.allow)
			}
			if tc.status == http.StatusNotFound {
				assertJSONMessage(t, rec, http.StatusNotFound, "Not found.")
			}
			if tc.status == http.StatusMethodNotAllowed {
				assertJSONMessage(t, rec, http.StatusMethodNotAllowed, "Method not allowed.")
			}
		})
	}
}

func TestRefusesUncleanPathsWithoutForwardingThem(t *testing.T) {
	paths := []string{
		"/v1/auth/../auth/login",
		"/v1//auth/login",
		"/v1/auth/./login",
		"/v1/tree/../auth/me",
		"/v1/tree/%2e%2e/auth/me",
		"/v1/tree/a/../../secret",
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			h := newHarness(t)

			rec := h.do(http.MethodGet, p, "")

			assertJSONMessage(t, rec, http.StatusBadRequest, "Invalid path.")
			if h.upstream.callCount() != 0 {
				t.Error("an unclean path reached the upstream")
			}
		})
	}
}

func TestRefusesOversizeBodies(t *testing.T) {
	big := strings.Repeat("x", 65)

	t.Run("declared length", func(t *testing.T) {
		h := newHarness(t)

		rec := h.do(http.MethodPost, "/v1/open", big)

		assertJSONMessage(t, rec, http.StatusRequestEntityTooLarge, "Request body too large.")
		if h.upstream.callCount() != 0 {
			t.Error("the upstream was called for an oversize body")
		}
	})

	t.Run("streamed body of unknown length", func(t *testing.T) {
		h := newHarness(t)
		req := httptest.NewRequest(http.MethodPost, "/v1/open", io.NopCloser(strings.NewReader(big)))
		req.ContentLength = -1
		rec := httptest.NewRecorder()

		h.gateway.ServeHTTP(rec, req)

		assertJSONMessage(t, rec, http.StatusRequestEntityTooLarge, "Request body too large.")
	})

	t.Run("body at the limit is fine", func(t *testing.T) {
		h := newHarness(t)

		rec := h.do(http.MethodPost, "/v1/open", strings.Repeat("x", 64))

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
}

func TestReturnsBadGatewayWhenTheUpstreamIsDown(t *testing.T) {
	upstream := newStubUpstream(t, nil)
	h := newHarnessFor(t, upstream)
	upstream.server.Close()

	rec := h.do(http.MethodGet, "/v1/open", "")

	assertJSONMessage(t, rec, http.StatusBadGateway, "Bad gateway.")
}

func TestReturnsGatewayTimeoutWhenTheUpstreamIsTooSlow(t *testing.T) {
	upstream := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	h := newHarnessFor(t, upstream)

	start := time.Now()
	rec := h.do(http.MethodGet, "/v1/open", "")
	elapsed := time.Since(start)

	assertJSONMessage(t, rec, http.StatusGatewayTimeout, "Upstream timed out.")
	if elapsed > 2*time.Second {
		t.Errorf("took %v, want it to give up near the 300ms upstream timeout", elapsed)
	}
}

func TestServesHealthWithoutAnyDependency(t *testing.T) {
	upstream := newStubUpstream(t, nil)
	h := newHarnessFor(t, upstream)
	upstream.server.Close()
	h.verifier.err = errors.New("down")
	h.limiter.err = errors.New("down")

	rec := h.do(http.MethodGet, "/health", "")

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Errorf("response = %d %s, want 200 {\"status\":\"ok\"}", rec.Code, rec.Body)
	}
	if h.verifier.callCount() != 0 || len(h.limiter.callList()) != 0 {
		t.Error("the health check touched the verifier or limiter")
	}

	post := h.do(http.MethodPost, "/health", "")
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /health = %d Allow %q, want 405 with GET, HEAD", post.Code, post.Header().Get("Allow"))
	}
}

func TestAccessLogRecordsTheRequestWithoutSecrets(t *testing.T) {
	h := newHarness(t)

	h.do(http.MethodGet, "/v1/auth/me?api_key=hunter2", "", "Authorization", "Bearer super.secret.token")

	logged := h.logs.String()
	for _, want := range []string{"method=GET", "path=/v1/auth/me", "route=protected", "status=200", "client_ip=192.0.2.1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("access log missing %q:\n%s", want, logged)
		}
	}
	for _, secret := range []string{"super.secret.token", "hunter2", "api_key"} {
		if strings.Contains(logged, secret) {
			t.Errorf("access log leaks %q:\n%s", secret, logged)
		}
	}
}

func TestAccessLogRecordsTheStatusOfGatewayErrors(t *testing.T) {
	h := newHarness(t)

	h.do(http.MethodGet, "/v1/nope", "")

	if !strings.Contains(h.logs.String(), "status=404") {
		t.Errorf("access log missing status=404:\n%s", h.logs)
	}
}
