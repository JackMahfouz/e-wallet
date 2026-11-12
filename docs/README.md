# E-Wallet: authentication, Identity, API gateway, ledger and wallet

These pages document the components that exist today. The other services in the
[system design](../E-Wallet%20Micro-Service%20System%20Design.md) (payment, fee engine, KYC, ...) are not built yet.

| Page | What it covers |
| --- | --- |
| [API gateway](api-gateway.md) | The public entry point: routing, token checks, rate limiting, and the YAML configuration |
| [Identity service](identity-service.md) | Registration, login, refresh, logout and the current user |
| [Auth library and service](libs-auth.md) | `libs/auth`: issues and verifies access tokens over internal gRPC |
| [Ledger service](ledger-service.md) | The append-only record of debits and credits per account, posted and read over internal gRPC |
| [Wallet service](wallet-service.md) | Users' wallets and accounts: transfers, deposits and withdrawals, daily limits with a two-step confirmation, holds, one-time codes; a hexagonal Go service that moves money through the ledger |
| [Shared money package](process-money.md) | `libs/process-money`: the one place amounts are cast between decimals and whole cents, used by the ledger and the wallet |

## The components

| Component | Language | Path | Role | Default address | Reachable from |
| --- | --- | --- | --- | --- | --- |
| API gateway | Go | [`gateway/`](../gateway) | Routes public requests, limits request rates, checks tokens | `:8080` (HTTP) | Clients |
| Identity | PHP, Laravel 13 | [`services/identity/`](../services/identity) | Owns users and refresh tokens; implements the auth endpoints | `127.0.0.1:8000` (HTTP) | The gateway only |
| Auth (`libs/auth`) | Go | [`libs/auth/`](../libs/auth) | Holds the signing key; issues and verifies RS256 access tokens | `127.0.0.1:9090` (gRPC) | Internal services only |
| Ledger | Go | [`services/ledger/`](../services/ledger) | Append-only debits and credits per account, with checkpointed balances | `127.0.0.1:9091` (gRPC) | Internal services only |
| Wallet | Go (hexagonal) | [`services/wallet/`](../services/wallet) | Wallets, accounts, limits, holds and the journal of money movements; calls the ledger | `127.0.0.1:8081` (HTTP), `127.0.0.1:9092` (gRPC) | HTTP: the gateway only. gRPC: internal services only |
| Money package | Go | [`libs/process-money/`](../libs/process-money) | Casts amounts between decimals and whole cents | none: a library | Imported by the ledger and the wallet |
| Redis | - | - | Rate limit counters (gateway) and login backoff (Identity) | `127.0.0.1:6379` | Gateway and Identity |
| PostgreSQL | - | - | The ledger's entries and checkpoints; the wallet's wallets, accounts, operations and holds (separate databases) | `127.0.0.1:5432` | The ledger and the wallet, each its own |

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

The ledger and the wallet sit beside this. The ledger is internal only: the services that move money call it
over gRPC, and it never goes through the gateway. The wallet has a public REST face (through the gateway) and an
internal gRPC face for other services; it asks the auth service to verify every token itself, and moves money by
calling the ledger.

```text
   Clients                                   Payment service (not built yet)
      |  /v1/wallets, /v1/accounts,                  |  gRPC Credit, Debit, Transfer,
      v  /v1/transfers, /v1/operations               v  CreateHold, ReleaseHold, GetAccount
 +-------------+  HTTP   +---------------------------------------+  gRPC Verify   +----------------+
 | API gateway | ------> |                Wallet                 | -------------> |   libs/auth    |
 |    :8080    |         |  REST :8081   |   internal gRPC :9092 |                |      :9090     |
 +-------------+         +-------+-----------------+-------------+                +----------------+
                                 | wallets, accounts, operations, holds
                                 | (its own PostgreSQL database)
                                 v
                         gRPC Debit, Credit, GetBalance (the operation id is the transaction id)
                                 |
                                 v
                          +--------------+
                          |    Ledger    |
                          |    :9091     | -- entries, checkpoints (PostgreSQL, append-only)
                          +--------------+
```

Both the ledger and the wallet use the [shared money package](process-money.md) to cast amounts.

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

**Transfer money** (`POST /v1/transfers`, with `Authorization`, `X-OTP` and `Idempotency-Key` headers)

1. The gateway rate limits the route and checks the token. The wallet verifies the token again with the auth service, checks the one-time code and that the source account is the caller's.
2. The wallet locks the wallet and account rows, checks the account is active, that the money is available (asking the ledger for the balance) and whether the spend would go over a daily limit. Over a limit, it saves the operation as awaiting confirmation and answers `409 confirmation_required`; nothing moves.
3. Otherwise it **commits the operation to its journal as `pending`**, then asks the ledger to debit the source and credit the destination, each call carrying the operation's id as its transaction id. It answers `201` once both are done.
4. If the ledger cannot be reached part-way, it answers `202`; the recovery job repeats the (idempotent) calls until they succeed, or, once five working days have passed, refunds the debit and fails the operation. See [Wallet service](wallet-service.md#how-money-reaches-the-ledger-and-what-happens-when-something-fails).

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
- **The wallet trusts the token, not the gateway:** it asks the auth service to verify every bearer token itself and ignores `X-User-ID`. Another user's account is a `404`, never a `403`.
- **One-time codes on sensitive actions** (deposit, withdrawal, transfer, confirming over a limit, closing, unfreezing, changing a limit). Until `libs/auth` issues real codes the code is one configured static value, which is a shared secret and **not a second factor**; the gateway's rate limit is the only brake on guessing it.
- **Money cannot be created by accident:** simulated funding, which creates ledger money with nothing behind it, is off unless `WALLET_FUNDING_MODE=simulated`.
- **No money lost between the wallet and the ledger:** every operation is journalled before the ledger is touched, every ledger call carries the operation's id (so it is applied once however often it is repeated), and a refund is a credit with the debit's own id. An operation is only failed when that cannot lose money.
- **Internal-only wallet gRPC:** the same trust model as the ledger: keep it off the gateway and restrict callers with the mesh.

## Run everything locally

Prerequisites: Go 1.26, PHP 8.3 or newer with the `redis` and `grpc` extensions, Composer, Redis 7,
`openssl`, and PostgreSQL 13 or newer (or Docker) for the ledger and the wallet. Identity cannot serve any auth request without the `grpc` extension (see
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

# 6. Wallet (needs the auth service (2) and the ledger (5) running, and its own PostgreSQL)
cd services/wallet
make test-db-up                          # a second throwaway PostgreSQL, on 127.0.0.1:55433
make run-dev                             # static one-time code 123456 and SIMULATED funding: development only
# REST on 127.0.0.1:8081 (the gateway routes /v1/wallets, /v1/accounts, /v1/transfers and /v1/operations to it),
# gRPC on 127.0.0.1:9092; see docs/wallet-service.md
```

The wallet identifies users by UUID, and Identity's ids are UUIDs, so use tokens from Identity (see the
[upgrade note](identity-service.md#run-it) if you have an older Identity database).

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

And the wallet (with the `make run-dev` settings above; money calls need the one-time code and an idempotency key):

```bash
H=(-H "Authorization: Bearer $ACCESS_TOKEN" -H 'Content-Type: application/json')

ACCOUNT=$(curl -s -X POST $GW/v1/accounts "${H[@]}" -d '{"name":"Current"}' | jq -r .id)
curl -s -X POST $GW/v1/accounts/$ACCOUNT/deposits "${H[@]}" \
  -H 'X-OTP: 123456' -H "Idempotency-Key: $(uuidgen)" -d '{"amount":100}'
curl -s $GW/v1/wallets/me "${H[@]}"
```

More, including limits and the over-limit confirmation, in [Wallet service](wallet-service.md#run-it).

## Run the tests

| Component | Command | Needs |
| --- | --- | --- |
| `libs/auth` | `cd libs/auth && make test` | Go |
| Gateway | `cd gateway && go test -race ./...` | Go (Redis is simulated in-process) |
| Identity | `cd services/identity && php artisan test --compact` | PHP with `redis`, and a running Redis (login tests use it) |
| Ledger | `cd services/ledger && make test-db-up && make test` | Go and a real PostgreSQL (Docker is easiest); `make test-db-down` afterwards |
| Money package | `cd libs/process-money && make test` | Go |
| Wallet | `cd services/wallet && make test-db-up && make test` | Go and a real PostgreSQL (Docker is easiest); `make test-db-down` afterwards |

## Where each component is configured

| Component | Configuration |
| --- | --- |
| Gateway | A YAML file, [`gateway/gateway.yaml`](../gateway/gateway.yaml) (`-config` flag or `GATEWAY_CONFIG`) |
| Identity | Environment variables, see [`.env.example`](../services/identity/.env.example) and [`config/jwt.php`](../services/identity/config/jwt.php) |
| Auth | Environment variables `AUTH_*`, see [libs/auth](libs-auth.md#configuration) |
| Ledger | Environment variables `LEDGER_*`, see [Ledger service](ledger-service.md#configuration) |
| Wallet | Environment variables `WALLET_*`, see [Wallet service](wallet-service.md#configuration) |

## Differences from the system design document

- **Gateway:** the design describes an Envoy configuration. The gateway is a Go program driven by YAML instead. The empty `envoy.yaml`, `rate-limit/` and `auth-filter/` placeholders are still in `gateway/`.
- **Tokens:** tokens are issued by a separate Go service (`libs/auth`) over gRPC, not by Laravel Passport inside Identity. They are RS256 JWTs with a 15-minute lifetime, as in the design.
- **Refresh tokens:** the design allows 30 days; the default here is `JWT_REFRESH_TTL=15` minutes. It is a setting, so change it with the environment variable.
- **Identity data model:** users have a UUID `id`, `name`, `email` and `password` (the Laravel default, with UUID ids so the wallet can identify users by them). Phone numbers, user status, the `sessions` table, MFA and OAuth2 clients from the design are not built.
- **Ledger amounts:** the design stores `numeric(18,6)`. The ledger stores whole cents in a `bigint` (amount x 100 on the way in, / 100 on the way out), because the API takes floats and integer storage keeps every sum exact. Amounts must be a whole number of cents. The casting is a shared package, [`libs/process-money`](process-money.md), used by the ledger and the wallet.
- **Ledger balance:** the design's `balance_after` column is not stored. A balance is the latest checkpoint (month, quarter, year or five-year) plus the entries after it. There is also no `currency` column, and `account_id` is a UUID rather than text.
- **Ledger writes:** entries are posted over gRPC (`Debit`, `Credit`), not consumed from the `ledger.entries` Kafka topic. The `events/` package is an empty placeholder.
- **Wallet:** the design has one wallet per user holding a `balance`, a `hold_balance` and a `currency`. Here a wallet has several accounts, there is no stored balance (the ledger is the only record), no currency column, and the layout is hexagonal rather than the design's `service`/`handler`/`store`. Limits are soft, with a strict two-step confirmation to go over them; every sensitive action needs a one-time code; and the wallet exposes user-facing transfers, deposits and withdrawals itself (the Payment service, not built, will use its gRPC API). There is no `wallet.events` Kafka topic and no velocity limit yet. See [Wallet service](wallet-service.md#differences-from-the-system-design).
- **Public key distribution:** not needed, because services verify through `libs/auth` instead of holding the public key. Vault is not integrated.

## Not built yet

Payment and the other services; Kafka events from the wallet and the ledger; real one-time codes (the wallet uses a static one until `libs/auth` issues them) and a real funding gateway (it is simulated, and off by default); TLS termination; metrics and tracing; Dockerfiles,
Helm charts, compose and CI (all still empty skeletons); an Istio policy that limits who may call
`Issue` or post to the ledger; token revocation before expiry; refresh token cleanup; an atomic ledger transfer call, runtime reconciliation of checkpoints, and a reconciliation job comparing the wallet's journal with the ledger (they agree in the end-to-end runs, but nothing checks it at runtime).
