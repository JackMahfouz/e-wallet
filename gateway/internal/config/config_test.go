package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/gateway/internal/config"
)

const minimal = `
upstreams:
  identity:
    url: http://identity:8000
routes:
  - name: login
    methods: [post]
    path: /v1/auth/login
    upstream: identity
`

func TestCommittedConfigIsValid(t *testing.T) {
	cfg, err := config.Load("../../gateway.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := map[string]struct {
		method string
		auth   bool
		limit  bool
	}{}
	for _, route := range cfg.Routes {
		if route.Upstream != "identity" {
			t.Errorf("route %s goes to %q, want identity", route.Name, route.Upstream)
		}
		got[route.Path] = struct {
			method string
			auth   bool
			limit  bool
		}{route.Methods[0], route.Auth, route.RateLimit != nil}
	}

	want := map[string]struct {
		method string
		auth   bool
		limit  bool
	}{
		"/v1/auth/register": {"POST", false, true},
		"/v1/auth/login":    {"POST", false, true},
		"/v1/auth/refresh":  {"POST", false, true},
		"/v1/auth/logout":   {"POST", true, false},
		"/v1/auth/me":       {"GET", true, false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d routes, want %d: %v", len(got), len(want), got)
	}
	for path, w := range want {
		if got[path] != w {
			t.Errorf("%s = %+v, want %+v", path, got[path], w)
		}
	}
	if cfg.Upstreams["identity"].PathPrefix != "/api" {
		t.Errorf("identity path_prefix = %q, want /api", cfg.Upstreams["identity"].PathPrefix)
	}
	if cfg.Redis == nil {
		t.Error("redis block missing from the default config")
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cfg.Server.Listen != ":8080" || cfg.Server.ReadHeaderTimeout != 5*time.Second ||
		cfg.Server.ShutdownTimeout != 15*time.Second || cfg.Server.MaxBodyBytes != 1<<20 {
		t.Errorf("server defaults = %+v", cfg.Server)
	}
	if cfg.Auth.Addr != "127.0.0.1:9090" || cfg.Auth.Timeout != 2*time.Second {
		t.Errorf("auth defaults = %+v", cfg.Auth)
	}
	if cfg.Redis != nil {
		t.Errorf("redis = %+v, want nil when the block is absent", cfg.Redis)
	}
	if cfg.Upstreams["identity"].Timeout != 10*time.Second {
		t.Errorf("upstream timeout = %v, want 10s", cfg.Upstreams["identity"].Timeout)
	}
	if got := cfg.Routes[0].Methods[0]; got != "POST" {
		t.Errorf("method = %q, want it upper-cased to POST", got)
	}
}

func TestParseAppliesRedisDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte(minimal + "redis:\n  addr: localhost:6379\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	r := cfg.Redis
	if r.Timeout != 100*time.Millisecond || r.KeyPrefix != "gateway:ratelimit:" || r.RetryInterval != 5*time.Second {
		t.Errorf("redis defaults = %+v", r)
	}
}

func TestParseReadsDurationsAndRateLimits(t *testing.T) {
	cfg, err := config.Parse([]byte(`
upstreams:
  identity: {url: "http://identity:8000", path_prefix: /api, timeout: 750ms}
routes:
  - {name: login, methods: [POST], path: /v1/auth/login, upstream: identity, rate_limit: {requests: 3, window: 90s}}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got := cfg.Upstreams["identity"].Timeout; got != 750*time.Millisecond {
		t.Errorf("timeout = %v, want 750ms", got)
	}
	if rl := cfg.Routes[0].RateLimit; rl.Requests != 3 || rl.Window != 90*time.Second {
		t.Errorf("rate_limit = %+v", rl)
	}
}

func TestRoutePrefixDetection(t *testing.T) {
	cfg, err := config.Parse([]byte(`
upstreams: {identity: {url: "http://identity:8000"}}
routes:
  - {name: tree, methods: [GET], path: /v1/things/, upstream: identity}
  - {name: leaf, methods: [GET], path: /v1/thing, upstream: identity}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !cfg.Routes[0].IsPrefix() || cfg.Routes[1].IsPrefix() {
		t.Errorf("IsPrefix = %v, %v; want true, false", cfg.Routes[0].IsPrefix(), cfg.Routes[1].IsPrefix())
	}
}

func TestParseRejectsInvalidConfigs(t *testing.T) {
	route := func(extra string) string {
		return minimal + extra
	}

	tests := map[string]struct {
		yaml    string
		message string
	}{
		"empty file":            {"", "config is empty"},
		"unknown top-level key": {minimal + "serverr: {}\n", "field serverr not found"},
		"unknown route key":     {strings.Replace(minimal, "upstream: identity", "upstream: identity\n    ratelimit: {}", 1), "field ratelimit not found"},
		"bad duration":          {strings.Replace(minimal, "url: http://identity:8000", "url: http://identity:8000\n    timeout: soon", 1), "parse config"},
		"negative duration":     {strings.Replace(minimal, "url: http://identity:8000", "url: http://identity:8000\n    timeout: -5s", 1), "upstreams.identity.timeout"},
		"no routes":             {"upstreams: {identity: {url: \"http://identity:8000\"}}\n", "routes: at least one route is required"},
		"no upstreams":          {"routes:\n  - {name: a, methods: [GET], path: /a, upstream: identity}\n", "upstreams: at least one upstream is required"},
		"unknown upstream":      {strings.Replace(minimal, "upstream: identity", "upstream: wallet", 1), `routes[0].upstream: "wallet" is not defined`},
		"relative upstream url": {strings.Replace(minimal, "http://identity:8000", "identity:8000", 1), "upstreams.identity.url"},
		"ftp upstream url":      {strings.Replace(minimal, "http://identity:8000", "ftp://identity", 1), "upstreams.identity.url"},
		"upstream url path":     {strings.Replace(minimal, "http://identity:8000", "http://identity:8000/api", 1), "use path_prefix"},
		"path prefix slash":     {strings.Replace(minimal, "url: http://identity:8000", "url: http://identity:8000\n    path_prefix: /api/", 1), "upstreams.identity.path_prefix"},
		"path prefix relative":  {strings.Replace(minimal, "url: http://identity:8000", "url: http://identity:8000\n    path_prefix: api", 1), "upstreams.identity.path_prefix"},
		"route without name":    {strings.Replace(minimal, "name: login", "name: \"\"", 1), "routes[0].name: is required"},
		"route without methods": {strings.Replace(minimal, "methods: [post]", "methods: []", 1), "routes[0].methods: at least one method"},
		"unsupported method":    {strings.Replace(minimal, "methods: [post]", "methods: [TRACE]", 1), `"TRACE" is not a supported method`},
		"relative path":         {strings.Replace(minimal, "path: /v1/auth/login", "path: v1/auth/login", 1), `must start with "/"`},
		"reserved health path":  {strings.Replace(minimal, "path: /v1/auth/login", "path: /health", 1), "reserved for the gateway health check"},
		"wildcard path":         {strings.Replace(minimal, "path: /v1/auth/login", "path: /v1/{id}", 1), "only exact paths and trailing-slash prefixes"},
		"unclean path":          {strings.Replace(minimal, "path: /v1/auth/login", "path: /v1/../login", 1), "must be a clean path"},
		"double slash path":     {strings.Replace(minimal, "path: /v1/auth/login", "path: /v1//login", 1), "must be a clean path"},
		"duplicate route name":  {route("  - {name: login, methods: [GET], path: /other, upstream: identity}\n"), `"login" is already used`},
		"duplicate method+path": {route("  - {name: again, methods: [POST], path: /v1/auth/login, upstream: identity}\n"), `"POST /v1/auth/login" is already defined by routes[0]`},
		"zero rate limit":       {route("  - {name: r, methods: [GET], path: /r, upstream: identity, rate_limit: {requests: 0, window: 1m}}\n"), "rate_limit.requests: must be greater than 0"},
		"rate limit no window":  {route("  - {name: r, methods: [GET], path: /r, upstream: identity, rate_limit: {requests: 5}}\n"), "rate_limit.window: must be a positive duration"},
		"redis without addr":    {minimal + "redis:\n  db: 1\n", "redis.addr: is required"},
		"redis negative db":     {minimal + "redis:\n  addr: localhost:6379\n  db: -1\n", "redis.db: must not be negative"},
		"bad listen address":    {minimal + "server:\n  listen: nonsense\n", "server.listen"},
		"negative body limit":   {minimal + "server:\n  max_body_bytes: -1\n", "server.max_body_bytes"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := config.Parse([]byte(tc.yaml))

			if err == nil {
				t.Fatalf("Parse succeeded with %+v, want an error", cfg)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %q, want it to contain %q", err, tc.message)
			}
		})
	}
}

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	_, err := config.Parse([]byte(`
upstreams:
  identity: {url: "nope"}
routes:
  - {name: a, methods: [GET], path: relative, upstream: missing}
`))

	var validation *config.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	if len(validation.Problems) < 3 {
		t.Errorf("problems = %v, want the url, path and upstream problems together", validation.Problems)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	_, err := config.Load("does-not-exist.yaml")

	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Errorf("err = %v, want a read error", err)
	}
}

func TestUpstreamTargetParsesTheURL(t *testing.T) {
	cfg, err := config.Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	target := cfg.Upstreams["identity"].Target()

	if target.Scheme != "http" || target.Host != "identity:8000" {
		t.Errorf("target = %v", target)
	}
}
