// Package config loads and validates the gateway's YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// HealthPath is served by the gateway itself and cannot be routed.
const HealthPath = "/health"

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
}

// Config is the whole gateway configuration.
type Config struct {
	Server    Server              `yaml:"server"`
	Auth      Auth                `yaml:"auth"`
	Redis     *Redis              `yaml:"redis"`
	Upstreams map[string]Upstream `yaml:"upstreams"`
	Routes    []Route             `yaml:"routes"`
}

// Server configures the listener.
type Server struct {
	Listen            string        `yaml:"listen"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	MaxBodyBytes      int64         `yaml:"max_body_bytes"`
	// ClientIPHeader names a header (for example X-Forwarded-For) set by exactly one trusted
	// proxy in front of the gateway. Empty means the client is the socket peer.
	ClientIPHeader string `yaml:"client_ip_header"`
}

// Auth is the internal libs/auth gRPC service used to verify tokens.
type Auth struct {
	Addr    string        `yaml:"addr"`
	Timeout time.Duration `yaml:"timeout"`
}

// Redis holds the rate limit counters. When the block is absent the limiter is in-memory only.
type Redis struct {
	Addr          string        `yaml:"addr"`
	DB            int           `yaml:"db"`
	TLS           bool          `yaml:"tls"`
	PasswordFile  string        `yaml:"password_file"`
	Timeout       time.Duration `yaml:"timeout"`
	KeyPrefix     string        `yaml:"key_prefix"`
	RetryInterval time.Duration `yaml:"retry_interval"`
}

// Upstream is a backend service the gateway proxies to.
type Upstream struct {
	URL        string        `yaml:"url"`
	PathPrefix string        `yaml:"path_prefix"`
	Timeout    time.Duration `yaml:"timeout"`
}

// Target returns the parsed upstream base URL. It is valid once Parse has succeeded.
func (u Upstream) Target() *url.URL {
	target, _ := url.Parse(u.URL)

	return target
}

// Route maps a public method and path to an upstream.
type Route struct {
	Name     string   `yaml:"name"`
	Methods  []string `yaml:"methods"`
	Path     string   `yaml:"path"`
	Upstream string   `yaml:"upstream"`
	// Auth requires a valid bearer token.
	Auth      bool       `yaml:"auth"`
	RateLimit *RateLimit `yaml:"rate_limit"`
}

// IsPrefix reports whether the route matches every path beneath it (the path ends in "/").
func (r Route) IsPrefix() bool { return strings.HasSuffix(r.Path, "/") }

// RateLimit allows Requests per client within each Window.
type RateLimit struct {
	Requests int           `yaml:"requests"`
	Window   time.Duration `yaml:"window"`
}

// ValidationError lists every problem found in a configuration.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid gateway config:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Load reads, defaults and validates the YAML file at filePath.
func Load(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	return Parse(data)
}

// Parse decodes YAML (unknown keys are an error), applies defaults and validates.
func Parse(data []byte) (*Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}

		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	setDefault(&c.Server.Listen, ":8080")
	setDefault(&c.Server.ReadHeaderTimeout, 5*time.Second)
	setDefault(&c.Server.ShutdownTimeout, 15*time.Second)
	setDefault(&c.Server.MaxBodyBytes, 1<<20)
	setDefault(&c.Auth.Addr, "127.0.0.1:9090")
	setDefault(&c.Auth.Timeout, 2*time.Second)

	if c.Redis != nil {
		setDefault(&c.Redis.Timeout, 100*time.Millisecond)
		setDefault(&c.Redis.KeyPrefix, "gateway:ratelimit:")
		setDefault(&c.Redis.RetryInterval, 5*time.Second)
	}

	for name, upstream := range c.Upstreams {
		setDefault(&upstream.Timeout, 10*time.Second)
		c.Upstreams[name] = upstream
	}

	for i := range c.Routes {
		for j, method := range c.Routes[i].Methods {
			c.Routes[i].Methods[j] = strings.ToUpper(strings.TrimSpace(method))
		}
	}
}

func setDefault[T comparable](value *T, fallback T) {
	var zero T
	if *value == zero {
		*value = fallback
	}
}

func (c *Config) validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		add("server.listen: %q is not a host:port address", c.Server.Listen)
	}
	positive := func(field string, d time.Duration) {
		if d <= 0 {
			add("%s: must be a positive duration", field)
		}
	}
	positive("server.read_header_timeout", c.Server.ReadHeaderTimeout)
	positive("server.shutdown_timeout", c.Server.ShutdownTimeout)
	if c.Server.MaxBodyBytes <= 0 {
		add("server.max_body_bytes: must be greater than 0")
	}
	positive("auth.timeout", c.Auth.Timeout)

	if c.Redis != nil {
		if c.Redis.Addr == "" {
			add("redis.addr: is required when the redis block is present")
		}
		if c.Redis.DB < 0 {
			add("redis.db: must not be negative")
		}
		positive("redis.timeout", c.Redis.Timeout)
		positive("redis.retry_interval", c.Redis.RetryInterval)
	}

	if len(c.Upstreams) == 0 {
		add("upstreams: at least one upstream is required")
	}
	names := make([]string, 0, len(c.Upstreams))
	for name := range c.Upstreams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		problems = append(problems, validateUpstream(name, c.Upstreams[name])...)
	}

	problems = append(problems, c.validateRoutes()...)

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}

	return nil
}

func validateUpstream(name string, upstream Upstream) []string {
	var problems []string
	field := "upstreams." + name

	target, err := url.Parse(upstream.URL)
	switch {
	case err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "":
		problems = append(problems, fmt.Sprintf("%s.url: %q must be an absolute http or https URL", field, upstream.URL))
	case (target.Path != "" && target.Path != "/") || target.RawQuery != "":
		problems = append(problems, fmt.Sprintf("%s.url: %q must not have a path or query (use path_prefix)", field, upstream.URL))
	}

	if upstream.PathPrefix != "" && (!strings.HasPrefix(upstream.PathPrefix, "/") || strings.HasSuffix(upstream.PathPrefix, "/")) {
		problems = append(problems, fmt.Sprintf("%s.path_prefix: %q must start with \"/\" and not end with \"/\"", field, upstream.PathPrefix))
	}

	if upstream.Timeout <= 0 {
		problems = append(problems, field+".timeout: must be a positive duration")
	}

	return problems
}

func (c *Config) validateRoutes() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if len(c.Routes) == 0 {
		add("routes: at least one route is required")
	}

	seenNames := map[string]bool{}
	seenRoutes := map[string]string{}

	for i, route := range c.Routes {
		field := fmt.Sprintf("routes[%d]", i)

		switch {
		case route.Name == "":
			add("%s.name: is required", field)
		case seenNames[route.Name]:
			add("%s.name: %q is already used by another route", field, route.Name)
		default:
			seenNames[route.Name] = true
		}

		if problem := validatePath(route.Path); problem != "" {
			add("%s.path: %q %s", field, route.Path, problem)
		}

		if len(route.Methods) == 0 {
			add("%s.methods: at least one method is required", field)
		}
		for _, method := range route.Methods {
			if !validMethods[method] {
				add("%s.methods: %q is not a supported method", field, method)
				continue
			}
			signature := method + " " + route.Path
			if first, dup := seenRoutes[signature]; dup {
				add("%s: %q is already defined by %s", field, signature, first)
				continue
			}
			seenRoutes[signature] = field
		}

		if _, ok := c.Upstreams[route.Upstream]; !ok {
			add("%s.upstream: %q is not defined under upstreams", field, route.Upstream)
		}

		if route.RateLimit != nil {
			if route.RateLimit.Requests <= 0 {
				add("%s.rate_limit.requests: must be greater than 0", field)
			}
			if route.RateLimit.Window <= 0 {
				add("%s.rate_limit.window: must be a positive duration", field)
			}
		}
	}

	return problems
}

// validatePath returns a description of what is wrong with a route path, or "" when it is fine.
func validatePath(p string) string {
	switch {
	case !strings.HasPrefix(p, "/"):
		return "must start with \"/\""
	case p == HealthPath:
		return "is reserved for the gateway health check"
	case strings.ContainsAny(p, "?#{}*"):
		return "must not contain ?, #, {, } or * (only exact paths and trailing-slash prefixes are supported)"
	}

	cleaned := path.Clean(p)
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}
	if cleaned != p {
		return "must be a clean path (no \"..\", \"//\" or \".\" segments)"
	}

	return ""
}
