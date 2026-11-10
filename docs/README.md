# E-Wallet: authentication, Identity, API gateway and ledger

These pages document the four components that exist today. The other services in the
[system design](../E-Wallet%20Micro-Service%20System%20Design.md) (wallet, payment, ...) are not built yet.

| Page | What it covers |
| --- | --- |
| [API gateway](api-gateway.md) | The public entry point: routing, token checks, rate limiting, and the YAML configuration |
| [Identity service](identity-service.md) | Registration, login, refresh, logout and the current user |
| [Auth library and service](libs-auth.md) | `libs/auth`: issues and verifies access tokens over internal gRPC |
| [Ledger service](ledger-service.md) | The append-only record of debits and credits per account, posted and read over internal gRPC |

## The components

| Component | Language | Path | Role | Default address | Reachable from |
| --- | --- | --- | --- | --- | --- |
| API gateway | Go | [`gateway/`](../gateway) | Routes public requests, limits request rates, checks tokens | `:8080` (HTTP) | Clients |
| Identity | PHP, Laravel 13 | [`services/identity/`](../services/identity) | Owns users and refresh tokens; implements the auth endpoints | `127.0.0.1:8000` (HTTP) | The gateway only |
| Auth (`libs/auth`) | Go | [`libs/auth/`](../libs/auth) | Holds the signing key; issues and verifies RS256 access tokens | `127.0.0.1:9090` (gRPC) | Internal services only |
| Ledger | Go | [`services/ledger/`](../services/ledger) | Append-only debits and credits per account, with checkpointed balances | `127.0.0.1:9091` (gRPC) | Internal services only |
| Redis | - | - | Rate limit counters (gateway) and login backoff (Identity) | `127.0.0.1:6379` | Gateway and Identity |
| PostgreSQL | - | - | The ledger's entries and checkpoints | `127.0.0.1:5432` | The ledger |

## How they fit together

```text
                 Internet
                    |   TLS is terminated in front of the gateway
                    v
             +--------------+       gRPC Verify        +----------------+
             | API gateway  | -----------------------> |   libs/auth    |
             |    :8080     |                          | :9090 internal |
             +------+-------+                          +--------^-------+
                    |  HTTP  /v1/auth/*  ->  /api/v1/auth/*     |
                    v                                           | gRPC Issue, Verify
             +--------------+ ----------------------------------+
             |   Identity   |
             |    :8000     | -- users, refresh_tokens (SQL database)
             +--------------+ -- login backoff (Redis)

             The gateway also keeps rate limit counters in Redis
             (and falls back to memory if Redis is down).
```

Only `libs/auth` holds the private key. Nothing else can create a valid access token, and nothing
else needs the key to check one: services ask `libs/auth` to verify.

The ledger sits beside this, not behind it. It is internal only: the services that move money
(the e-wallet and payment services, once built) call it over gRPC, and it never goes through the gateway.

```text
        e-wallet / payment services (not built yet)
                    |  gRPC Debit, Credit, GetBalance, ListEntries, ListCheckpoints
                    v
             +--------------+
             |    Ledger    |
             |    :9091     | -- entries, checkpoints (PostgreSQL, append-only)
             +--------------+
```

## Request flows

**Log in** (`POST /v1/auth/login`)

1. The gateway counts the request against the client's rate limit (10 per minute per IP), then forwards it to Identity.
2. Identity checks the login backoff in Redis, then the password.
3. Identity asks `libs/auth` to `Issue` an access token for the user, stores a hash of a new random refresh token, and returns both tokens.

**Call a protected endpoint** (`GET /v1/auth/me` with `Authorization: Bearer <access token>`)

1. The gateway asks `libs/auth` to `Verify` the token. A bad or expired token is answered with 401 and never reaches Identity.
2. The gateway adds `X-User-ID: <user id>` and forwards the request, still carrying the original `Authorization` header.
3. Identity verifies the token again through its own guard and loads the user.

**Refresh** (`POST /v1/auth/refresh`): Identity finds the live refresh token by its hash, revokes it,
and returns a new access and refresh token (rotation). Using an old refresh token again fails with 401.

**Log out** (`POST /v1/auth/logout`): Identity revokes the given refresh token. The access token
already issued stays valid until it expires (at most 15 minutes), because access tokens are stateless.

**Post to the ledger** (`Debit` or `Credit` over gRPC): the ledger takes a lock on the account, checks whether
the transaction was already posted, checks the balance (a debit larger than the balance is refused,
and every debit on a zero balance is), appends one entry and returns the new balance. The balance is
the account's latest checkpoint plus the entries after it. See [Ledger service](ledger-service.md).

## Security model in one place

- **Key custody:** the signing key is a file read only by `libs/auth`. It is not in the repo (`*.pem` is git-ignored).
- **Internal-only auth service:** `libs/auth` listens on loopback by default and has no application-level secret. In a cluster, keep it off the gateway and restrict callers with the mesh or network policy.
- **Identity headers:** the gateway removes any client-supplied `X-User-ID`, and any header with an underscore in its name, before forwarding. Only the gateway can say who the user is.
- **Fail closed:** if the auth service is unreachable the gateway returns 503 for protected routes, and Identity returns 500. Neither lets a request through unchecked.
- **Brute force:** the gateway rate limits register, login and refresh per client IP. Identity adds an exponential lockout on repeated failed logins per email and IP.
- **An append-only ledger:** the ledger's database refuses `UPDATE`, `DELETE` and `TRUNCATE` on its tables, so history can only be added to. Money is stored as whole cents in integers, and a debit can never take an account below zero, even with many concurrent debits across several servers.
- **Internal-only ledger:** like the auth service, it has no application-level secret. Keep it off the gateway and restrict who may post with the mesh or network policy.

## Run everything locally

Prerequisites: Go 1.26, PHP 8.3 or newer with the `redis` and `grpc` extensions, Composer, Redis 7,
`openssl`, and PostgreSQL 13 or newer (or Docker) for the ledger. Identity cannot serve any auth request without the `grpc` extension (see
[Troubleshooting](identity-service.md#troubleshooting)).

Start each in its own terminal, in this order:

```bash
# 1. Redis (skip if one is already running on 6379)
redis-server

# 2. Auth service
cd libs/auth
openssl genrsa -out auth.pem 2048
AUTH_PRIVATE_KEY_FILE=auth.pem go run ./cmd/server

# 3. Identity (first run also needs: composer install, .env, key, migrations)
cd services/identity
composer install
cp .env.example .env && php artisan key:generate
touch database/database.sqlite && php artisan migrate
php artisan serve                       # http://127.0.0.1:8000

# 4. Gateway
cd gateway
go run ./cmd/server -config gateway.yaml  # http://127.0.0.1:8080

# 5. Ledger (independent of the four above; needs a PostgreSQL, here a throwaway one in Docker)
cd services/ledger
make test-db-up
LEDGER_DATABASE_URL='postgres://postgres:ledger@127.0.0.1:55432/ledger?sslmode=disable' go run ./cmd/server
# gRPC on 127.0.0.1:9091; see docs/ledger-service.md for calling it
```

Then use the API through the gateway:

```bash
GW=http://127.0.0.1:8080

# Register (returns an access token and a refresh token)
curl -s -X POST $GW/v1/auth/register -H 'Content-Type: application/json' \
  -d '{"name":"Ada Lovelace","email":"ada@example.com","password":"secret-password"}'

# Log in, then call a protected endpoint with the access token
curl -s -X POST $GW/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"secret-password"}'
curl -s $GW/v1/auth/me -H "Authorization: Bearer $ACCESS_TOKEN"

# Refresh, then log out
curl -s -X POST $GW/v1/auth/refresh -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH_TOKEN\"}"
curl -s -X POST $GW/v1/auth/logout -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REFRESH_TOKEN\"}"
```

## Run the tests

| Component | Command | Needs |
| --- | --- | --- |
| `libs/auth` | `cd libs/auth && make test` | Go |
| Gateway | `cd gateway && go test -race ./...` | Go (Redis is simulated in-process) |
| Identity | `cd services/identity && php artisan test --compact` | PHP with `redis`, and a running Redis (login tests use it) |
| Ledger | `cd services/ledger && make test-db-up && make test` | Go and a real PostgreSQL (Docker is easiest); `make test-db-down` afterwards |

## Where each component is configured

| Component | Configuration |
| --- | --- |
| Gateway | A YAML file, [`gateway/gateway.yaml`](../gateway/gateway.yaml) (`-config` flag or `GATEWAY_CONFIG`) |
| Identity | Environment variables, see [`.env.example`](../services/identity/.env.example) and [`config/jwt.php`](../services/identity/config/jwt.php) |
| Auth | Environment variables `AUTH_*`, see [libs/auth](libs-auth.md#configuration) |
| Ledger | Environment variables `LEDGER_*`, see [Ledger service](ledger-service.md#configuration) |

## Differences from the system design document

- **Gateway:** the design describes an Envoy configuration. The gateway is a Go program driven by YAML instead. The empty `envoy.yaml`, `rate-limit/` and `auth-filter/` placeholders are still in `gateway/`.
- **Tokens:** tokens are issued by a separate Go service (`libs/auth`) over gRPC, not by Laravel Passport inside Identity. They are RS256 JWTs with a 15-minute lifetime, as in the design.
- **Refresh tokens:** the design allows 30 days; the default here is `JWT_REFRESH_TTL=15` minutes. It is a setting, so change it with the environment variable.
- **Identity data model:** users have an integer `id`, `name`, `email` and `password` (the Laravel default). Phone numbers, UUIDs, user status, the `sessions` table, MFA and OAuth2 clients from the design are not built.
- **Ledger amounts:** the design stores `numeric(18,6)`. The ledger stores whole cents in a `bigint` (amount x 100 on the way in, / 100 on the way out), because the API takes floats and integer storage keeps every sum exact. Amounts must be a whole number of cents.
- **Ledger balance:** the design's `balance_after` column is not stored. A balance is the latest checkpoint (month, quarter, year or five-year) plus the entries after it. There is also no `currency` column, and `account_id` is a UUID rather than text.
- **Ledger writes:** entries are posted over gRPC (`Debit`, `Credit`), not consumed from the `ledger.entries` Kafka topic. The `events/` package is an empty placeholder.
- **Public key distribution:** not needed, because services verify through `libs/auth` instead of holding the public key. Vault is not integrated.

## Not built yet

Wallet, payment and the other services (and so the accounts the ledger posts to); TLS termination; metrics and tracing; Dockerfiles,
Helm charts, compose and CI (all still empty skeletons); an Istio policy that limits who may call
`Issue` or post to the ledger; token revocation before expiry; refresh token cleanup; an atomic ledger transfer call and runtime reconciliation of checkpoints.
