# API gateway

The gateway is the public entry point. It is a small Go reverse proxy, driven entirely by one YAML
file, that decides which requests are allowed through and where they go. Today it fronts the
[Identity service](identity-service.md) and the [wallet service](wallet-service.md).

- **Path:** [`gateway/`](../gateway), module `github.com/JackMahfouz/e-wallet/gateway`
- **Default address:** `:8080`
- **Configuration:** [`gateway/gateway.yaml`](../gateway/gateway.yaml)
- **Depends on:** the [auth service](libs-auth.md) (to verify tokens) and Redis (rate limit counters, with an in-memory fallback)

## What it does for each request

In this order:

1. **Health.** `GET /health` is answered by the gateway itself (`{"status":"ok"}`), without touching any dependency.
2. **Path check.** A path that is not already clean (contains `..`, `//` or a `.` segment) is refused with `400`, never normalized and forwarded.
3. **Header clean-up.** Any client-supplied `X-User-ID`, and any header whose name contains an underscore, is removed.
4. **Body limit.** A declared body larger than `server.max_body_bytes` gets `413`; a streamed body is cut off at the limit.
5. **Route match.** No route for the path gives `404`; a route that does not take the method gives `405` with an `Allow` header.
6. **Rate limit**, if the route has one: over the limit gives `429` with `Retry-After`.
7. **Token check**, if the route has `auth: true`: the token is verified by the auth service. Rejected gives `401`; auth service unavailable gives `503`. A valid token adds `X-User-ID`.
8. **Proxy** to the route's upstream with the upstream's timeout. An unreachable upstream gives `502`; a slow one gives `504`.

The rate limit comes before the token check on purpose, so a flood of requests never reaches the auth service.

## Configuration reference

Unknown keys are an error, so a typo fails at startup instead of being ignored. Durations use Go
syntax (`500ms`, `5s`, `1m`). All problems found are reported together, with the field path.

### `server`

| Key | Default | Description |
| --- | --- | --- |
| `listen` | `:8080` | Address to listen on (`host:port`) |
| `read_header_timeout` | `5s` | How long a client may take to send request headers |
| `shutdown_timeout` | `15s` | How long in-flight requests get to finish on shutdown |
| `max_body_bytes` | `1048576` | Largest request body accepted (1 MiB) |
| `client_ip_header` | empty | Header holding the client address, set by exactly one trusted proxy in front of the gateway (for example `X-Forwarded-For`). Empty means the client is whoever connected. See [Client address](#client-address). |

### `auth`

The internal [auth service](libs-auth.md), used on routes with `auth: true`.

| Key | Default | Description |
| --- | --- | --- |
| `addr` | `127.0.0.1:9090` | gRPC address (plaintext; keep it on the trusted network) |
| `timeout` | `2s` | Deadline for each `Verify` call |

### `redis` (optional)

Rate limit counters. **Omit the whole block to use the in-memory limiter only** (counts are then
per gateway instance, and a warning is logged at startup).

| Key | Default | Description |
| --- | --- | --- |
| `addr` | required | `host:port`, required if the block is present |
| `db` | `0` | Database number |
| `tls` | `false` | Connect over TLS (minimum TLS 1.2) |
| `password_file` | empty | Path to a file holding the password. Never put the password in the YAML. |
| `timeout` | `100ms` | Bound on connecting, reading and writing. Redis calls fail fast, with no retries. |
| `key_prefix` | `gateway:ratelimit:` | Prefix for every key |
| `retry_interval` | `5s` | While Redis is down, probe it at most this often |

### `upstreams`

A map of name to backend service.

| Key | Default | Description |
| --- | --- | --- |
| `url` | required | `http` or `https` base URL, with no path or query |
| `path_prefix` | empty | Prepended to every proxied path. Must start with `/` and not end with one. Identity uses `/api`, so `/v1/auth/login` becomes `/api/v1/auth/login`. |
| `timeout` | `10s` | Overall time limit for the upstream exchange, including streaming the response |

### `routes`

A list. Each route maps a public method and path to an upstream.

| Key | Required | Description |
| --- | --- | --- |
| `name` | yes | Unique. Used in logs and in rate limit keys. |
| `methods` | yes | One or more of `GET POST PUT PATCH DELETE HEAD OPTIONS` (case-insensitive). `HEAD` is not implied by `GET`. |
| `path` | yes | Must start with `/` and be clean. A path ending in `/` matches everything beneath it; any other path matches exactly. No wildcards, and `/health` is reserved. |
| `upstream` | yes | A key from `upstreams` |
| `auth` | no (`false`) | Require a valid bearer token |
| `rate_limit` | no | `requests` (greater than 0) allowed per `window` (a positive duration), per client address |

Method and path pairs must be unique across routes.

### The shipped configuration

[`gateway.yaml`](../gateway/gateway.yaml) has two upstreams, `identity` (`http://127.0.0.1:8000`, path prefix `/api`) and `wallet` (`http://127.0.0.1:8081`, no prefix, a 15 second timeout because a transfer waits on the ledger). The Identity routes:

| Route | Method and path | Auth | Rate limit |
| --- | --- | --- | --- |
| `auth-register` | `POST /v1/auth/register` | no | 10 per minute |
| `auth-login` | `POST /v1/auth/login` | no | 10 per minute |
| `auth-refresh` | `POST /v1/auth/refresh` | no | 10 per minute |
| `auth-logout` | `POST /v1/auth/logout` | yes | none |
| `auth-me` | `GET /v1/auth/me` | yes | none |

The wallet routes. Every one needs a token (the wallet checks it again itself and never trusts `X-User-ID`).
Reads are unlimited; anything that changes state, and so may carry a one-time code in `X-OTP`, is rate limited to 30
per minute per client, which is also the only brake on guessing the static one-time code (see the
[wallet service](wallet-service.md#authentication-and-one-time-codes)):

| Route | Method and path | Auth | Rate limit |
| --- | --- | --- | --- |
| `wallet-get` | `GET /v1/wallets/me` | yes | none |
| `wallet-update` | `PATCH /v1/wallets/me` | yes | 30 per minute |
| `accounts-create` | `POST /v1/accounts` | yes | 30 per minute |
| `accounts-read` | `GET /v1/accounts/...` (an account, and its transactions) | yes | none |
| `accounts-write` | `POST`, `PATCH`, `DELETE` `/v1/accounts/...` (deposits, withdrawals, rename and limits, close) | yes | 30 per minute |
| `transfers-create` | `POST /v1/transfers` | yes | 30 per minute |
| `operations-read` | `GET /v1/operations/...` | yes | none |
| `operations-write` | `POST /v1/operations/...` (confirm and cancel) | yes | 30 per minute |

The two `/v1/accounts/` and two `/v1/operations/` entries are prefix routes (a trailing slash matches everything
beneath it) split by method, so reads and writes can have different limits. The wallet's internal gRPC port is
deliberately not routed.

### Adding a service

Add an upstream and its routes; no code changes are needed. This is how the wallet was added (the shipped entries above are the real ones; this is a smaller illustration):

```yaml
upstreams:
  wallet:
    url: http://wallet:8081
    timeout: 5s

routes:
  - name: wallet-me
    methods: [GET]
    path: /v1/wallets/me
    upstream: wallet
    auth: true

  - name: wallet-tree          # trailing slash: everything under /v1/wallets/
    methods: [GET, POST]
    path: /v1/wallets/
    upstream: wallet
    auth: true
    rate_limit: {requests: 60, window: 1m}
```

When prefix routes overlap, the longest path wins. Restart the gateway to apply changes; there is no hot reload.

## Authentication

On a route with `auth: true` the request must carry `Authorization: Bearer <token>` (the scheme is
case-insensitive). The gateway sends the token to the auth service's `Verify` call.

| Outcome | Response |
| --- | --- |
| Valid token | The request continues with `X-User-ID: <subject>` added. The original `Authorization` header is forwarded unchanged, so the upstream can verify it again. |
| Missing header, wrong scheme, empty token, or rejected token | `401 {"message":"Unauthenticated."}` with `WWW-Authenticate: Bearer`. The upstream is not called. |
| Auth service unreachable, timed out or misbehaving | `503 {"message":"Authentication service unavailable."}`. The upstream is not called. |

The gateway **fails closed**: if it cannot check a token, it does not let the request through.

Upstream services can trust `X-User-ID` only because the gateway removes any client-supplied copy
first, including underscore spellings such as `X_User_ID` that PHP and other CGI-style servers treat
as the same header.

## Rate limiting

Each rate-limited request is counted against the key `<key_prefix><route name>:<client address>`
in a **fixed window** that starts at the first request and lasts `window`. Once `requests` is
exceeded, requests get:

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 42
{"message":"Too many requests."}
```

`Retry-After` is the time left in the window, rounded up to whole seconds (never less than 1).
Fixed windows are simple and cheap, but a client can make up to twice the limit across a window
boundary.

**With Redis.** Each request runs `INCR`, `PEXPIRE ... NX` and `PTTL` in one `MULTI`. That is atomic,
sets the expiry only when the key has none (so traffic never extends a window, and a counter can never be
left without an expiry), and the counters are shared by every gateway instance.

**When Redis is down.** The gateway switches to an in-memory limiter with the same limits and
logs `redis rate limiter unavailable, using the in-memory limiter` once. While Redis is down it is
probed at most once per `retry_interval`, with only one probe in flight; every other request goes
straight to memory, so an outage adds no per-request delay. When a probe succeeds the gateway
switches back and logs `redis rate limiter recovered`. Counters are not carried across a switch, and
in-memory counts are per instance, so limits are looser (up to N times with N instances) for the
duration of an outage. A request cancelled by its client is not treated as an outage.

**Without a `redis` block** the in-memory limiter is always used.

In the unexpected case that the limiter itself returns an error, the request is served and the
error is logged: rate limiting is best effort.

### Client address

Limits are per client address. By default that is the address that connected to the gateway.

If a load balancer sits in front, every request appears to come from it and shares one limit. Set
`server.client_ip_header` to the header the balancer writes (for example `X-Forwarded-For`). The
gateway then uses the **last** address in that header, the one the trusted proxy appended, because
earlier entries are client-controlled. The value must be a valid IP address, otherwise the socket
address is used.

Only set it when exactly one trusted proxy writes that header. With direct client connections, leave
it empty: a client could otherwise choose its own rate limit key.

## Headers sent to the upstream

- `X-User-ID` is set from a verified token and nowhere else.
- Headers with underscores in their names are dropped.
- `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto` are set by the gateway. A client's own
  `X-Forwarded-*` headers are replaced, except that with `client_ip_header` set the incoming
  `X-Forwarded-For` chain is kept and the connecting address appended.
- Hop-by-hop headers (such as `Connection`) are removed. Method, body, query string and every other header are forwarded.
- The upstream sees its own host in `Host`; the original is in `X-Forwarded-Host`.

Upstream responses (status, headers, body) pass through unchanged, including Identity's own
`401`, `422` and `429` JSON.

## Responses produced by the gateway

All are JSON `{"message": "..."}`, the same shape Identity uses.

| Status | Message | When |
| --- | --- | --- |
| `400` | `Invalid path.` | The path contains `..`, `//` or a `.` segment |
| `401` | `Unauthenticated.` | Missing or rejected token on a route with `auth: true` |
| `404` | `Not found.` | No route for the path |
| `405` | `Method not allowed.` | The path exists but not for this method (`Allow` lists the methods) |
| `413` | `Request body too large.` | Body over `max_body_bytes` |
| `429` | `Too many requests.` | Over the route's rate limit (`Retry-After` is set) |
| `502` | `Bad gateway.` | The upstream could not be reached |
| `503` | `Authentication service unavailable.` | Token verification could not be completed |
| `504` | `Upstream timed out.` | The upstream did not answer within its `timeout` |

## Logging

One JSON line per request on standard output, plus warnings and errors for the events above:

```json
{"time":"2026-11-09T12:00:00Z","level":"INFO","msg":"request","method":"GET","path":"/v1/auth/me","route":"auth-me","status":200,"duration_ms":4,"client_ip":"203.0.113.7"}
```

`route` is empty when no route matched. The query string, headers, request bodies and tokens are
**never** logged.

## Run it

```bash
cd gateway
go run ./cmd/server -config gateway.yaml
# or
go build -o gateway ./cmd/server && ./gateway -config gateway.yaml
```

The config path is the `-config` flag, then the `GATEWAY_CONFIG` environment variable, then `gateway.yaml`
in the current directory.

- **Startup:** an invalid configuration exits with code 1 after logging every problem it found. They
  appear on a single log line, separated by `\n`:

  ```text
  ERROR gateway stopped error="invalid gateway config:\n  - routes[0].path: \"relative\" must start with \"/\"\n  - routes[0].upstream: \"missing\" is not defined under upstreams"
  ```

  Neither Redis nor the auth service has to be up at startup: if Redis is down a warning is logged
  and the in-memory limiter is used until it returns, and the auth connection is made on first use.
- **Shutdown:** `SIGINT` or `SIGTERM` stops accepting connections, lets in-flight requests finish for
  up to `shutdown_timeout`, and exits with code 0.

## Code layout

```text
gateway/
  gateway.yaml                the shipped configuration
  cmd/server/main.go          wiring: config, auth client, limiter, HTTP server, shutdown
  internal/config/            YAML types, defaults and validation
  internal/proxy/             router, request pipeline, reverse proxy, JSON errors
  internal/authfilter/        token verification client for the auth service (gRPC)
  internal/ratelimit/         limiter interface; Redis, in-memory and failover implementations
```

`libs/auth` is consumed through a `replace` directive in [`go.mod`](../gateway/go.mod), so the
gateway builds against the local checkout.

## Testing

```bash
cd gateway && go test -race ./...
```

No external services are needed. Redis is simulated in-process (miniredis), so window expiry is
tested without waiting. The tests cover:

- **configuration:** the shipped `gateway.yaml` stays valid; defaults; every validation failure; unknown keys
- **proxying:** the `/api` prefix, method, query and body; upstream responses passed through unchanged
- **authentication:** refused and fail-closed paths, `X-User-ID` set and spoofing removed (including underscore spellings), the original `Authorization` forwarded
- **rate limiting:** `429` and `Retry-After`, keys per route and client, the order against the token check, trusted-proxy handling
- **failover:** Redis stopped and restarted, one probe per interval, and one in-flight probe at a time
- **errors:** `404`, `405`, `400`, `413`, `502`, `504`, and that tokens and query strings never reach the log
- **auth client:** a real gRPC connection to a fake auth server, including slow and failing servers

## Limitations

- **No TLS.** Terminate TLS in front of the gateway.
- **No metrics or tracing**, and no HTTP `/metrics` endpoint.
- **No hot reload** of the configuration.
- **No retries or circuit breaking** towards upstreams, and no caching of token verification: each
  protected request makes one gRPC call to the auth service.
- **Rate limits are per client address only**, not per user, and use fixed windows.
- **During a Redis outage**, limits are per gateway instance.
- **Only Identity and the wallet are routed.** The ledger, the auth service and the wallet's gRPC port are internal only, so they are deliberately not routed; the other services in the design document are not built.
- **The system design document describes an Envoy gateway.** This Go gateway replaces that plan;
  the empty `envoy.yaml`, `rate-limit/` and `auth-filter/` placeholders are still in `gateway/`.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| Exits at start with `gateway stopped` and `invalid gateway config:` | Fix each field listed in the message (they are separated by `\n`). `field x not found` means an unknown or misspelled key. |
| `503` on protected routes | The auth service is down, `auth.addr` is wrong, or it is slower than `auth.timeout`. The log has `token verification failed`. |
| `502` | The upstream is down or its `url` is wrong |
| `504` | The upstream answered slower than its `timeout` |
| `404` for a URL that should work | Check the path: an exact route does not match a trailing slash or extra segments. Check the `methods` too (`405` if the method is wrong). |
| `400 Invalid path.` | The URL contains `..`, `//` or `/./`. `curl` normalizes these unless you pass `--path-as-is`. |
| `429` with `Too many requests.` | The gateway rate limit. (`Too many failed attempts. Try again later.` comes from Identity's login backoff.) |
| Everyone shares one rate limit | The gateway sits behind a proxy; set `server.client_ip_header` |
| Limits are not shared between instances | The `redis` block is missing, or Redis is unreachable (look for `using the in-memory limiter` in the log) |
