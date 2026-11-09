// Package proxy is the gateway's HTTP handler: it matches routes, applies rate limits and token
// checks, and reverse proxies to upstream services.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/JackMahfouz/e-wallet/gateway/internal/authfilter"
	"github.com/JackMahfouz/e-wallet/gateway/internal/config"
	"github.com/JackMahfouz/e-wallet/gateway/internal/ratelimit"
)

// Options are the Gateway's dependencies.
type Options struct {
	Config   *config.Config
	Verifier authfilter.Verifier
	Limiter  ratelimit.Limiter
	Logger   *slog.Logger
}

// Gateway is the http.Handler that serves every request.
type Gateway struct {
	cfg      *config.Config
	verifier authfilter.Verifier
	limiter  ratelimit.Limiter
	log      *slog.Logger
	router   *router
}

type upstream struct {
	timeout time.Duration
	proxy   *httputil.ReverseProxy
}

// New builds a Gateway from a validated configuration.
func New(opts Options) (*Gateway, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	keepForwardedChain := opts.Config.Server.ClientIPHeader != ""

	upstreams := make(map[string]*upstream, len(opts.Config.Upstreams))
	for name, cfg := range opts.Config.Upstreams {
		upstreams[name] = newUpstream(name, cfg, transport, keepForwardedChain, logger)
	}

	routes := make([]*route, 0, len(opts.Config.Routes))
	for _, cfg := range opts.Config.Routes {
		target, ok := upstreams[cfg.Upstream]
		if !ok {
			return nil, fmt.Errorf("route %q uses unknown upstream %q", cfg.Name, cfg.Upstream)
		}
		routes = append(routes, &route{Route: cfg, upstream: target})
	}

	return &Gateway{
		cfg:      opts.Config,
		verifier: opts.Verifier,
		limiter:  opts.Limiter,
		log:      logger,
		router:   newRouter(routes),
	}, nil
}

func newUpstream(name string, cfg config.Upstream, transport http.RoundTripper, keepForwardedChain bool, log *slog.Logger) *upstream {
	target := cfg.Target()

	return &upstream{
		timeout: cfg.Timeout,
		proxy: &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = cfg.PathPrefix + pr.In.URL.Path
				pr.Out.URL.RawPath = ""

				// ReverseProxy drops client-supplied X-Forwarded-* before Rewrite runs, so a client
				// can't forge them. Behind one trusted proxy its chain is worth keeping.
				if prior := pr.In.Header["X-Forwarded-For"]; keepForwardedChain && len(prior) > 0 {
					pr.Out.Header["X-Forwarded-For"] = prior
				}
				pr.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				var tooLarge *http.MaxBytesError

				switch {
				case errors.As(err, &tooLarge):
					writeError(w, http.StatusRequestEntityTooLarge, "Request body too large.")
				case errors.Is(err, context.DeadlineExceeded):
					log.Warn("upstream timed out", "upstream", name, "path", r.URL.Path)
					writeError(w, http.StatusGatewayTimeout, "Upstream timed out.")
				default:
					log.Error("upstream request failed", "upstream", name, "path", r.URL.Path, "error", err)
					writeError(w, http.StatusBadGateway, "Bad gateway.")
				}
			},
		},
	}
}

// ServeHTTP implements http.Handler and writes one access log line per request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	routeName := g.serve(recorder, r)

	// The query string is left out of the log on purpose: it can carry secrets.
	g.log.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"route", routeName,
		"status", recorder.status,
		"duration_ms", time.Since(start).Milliseconds(),
		"client_ip", clientIP(r, g.cfg.Server.ClientIPHeader),
	)
}

// serve handles one request and returns the name of the route it matched, if any.
//
// Order matters: unclean paths and client-supplied identity are dealt with first, the rate limit
// comes before the token check (so a flood never reaches the auth service), and the request is
// only proxied once every check has passed.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) string {
	if r.URL.Path == config.HealthPath {
		g.health(w, r)

		return "health"
	}

	if !isClean(r.URL.Path) {
		writeError(w, http.StatusBadRequest, "Invalid path.")

		return ""
	}

	// Only this gateway may say who the user is: drop anything the client sent, including
	// look-alike spellings that CGI-style upstreams would treat as the same header.
	r.Header.Del(userIDHeader)
	dropUnderscoreHeaders(r.Header)

	limit := g.cfg.Server.MaxBodyBytes
	if r.ContentLength > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "Request body too large.")

		return ""
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	matched, allowed := g.router.match(r.URL.Path, r.Method)
	if matched == nil {
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")
		} else {
			writeError(w, http.StatusNotFound, "Not found.")
		}

		return ""
	}

	if !g.allowedByRateLimit(w, r, matched) {
		return matched.Name
	}

	if matched.Auth && !g.authenticate(w, r) {
		return matched.Name
	}

	ctx, cancel := context.WithTimeout(r.Context(), matched.upstream.timeout)
	defer cancel()
	matched.upstream.proxy.ServeHTTP(w, r.WithContext(ctx))

	return matched.Name
}

// allowedByRateLimit applies the route's rate limit. It writes the 429 itself when refusing.
func (g *Gateway) allowedByRateLimit(w http.ResponseWriter, r *http.Request, matched *route) bool {
	if matched.RateLimit == nil {
		return true
	}

	key := matched.Name + ":" + clientIP(r, g.cfg.Server.ClientIPHeader)
	result, err := g.limiter.Allow(r.Context(), key, matched.RateLimit.Requests, matched.RateLimit.Window)
	if err != nil {
		// The configured limiter already falls back from Redis to memory, so an error here is
		// unexpected. Rate limiting is best effort: log it and serve the request.
		g.log.Error("rate limiter failed, allowing the request", "route", matched.Name, "error", err)

		return true
	}

	if result.Allowed {
		return true
	}

	w.Header().Set("Retry-After", retryAfterSeconds(result.RetryAfter))
	writeError(w, http.StatusTooManyRequests, "Too many requests.")

	return false
}

// authenticate checks the bearer token with the auth service and, when it is valid, tells the
// upstream who the user is. It writes the error response itself when refusing. If the auth
// service cannot be reached the request is refused (503): never let it through unchecked.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) bool {
	token := bearerToken(r)
	if token == "" {
		unauthenticated(w)

		return false
	}

	subject, err := g.verifier.Verify(r.Context(), token)
	switch {
	case errors.Is(err, authfilter.ErrUnauthenticated):
		unauthenticated(w)

		return false
	case err != nil:
		g.log.Error("token verification failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "Authentication service unavailable.")

		return false
	}

	// Assigned directly (not via Header.Set) to keep the header name exactly as documented.
	r.Header[userIDHeader] = []string{subject}

	return true
}

func unauthenticated(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "Unauthenticated.")
}

func (g *Gateway) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")

		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
