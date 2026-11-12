# Identity service

Identity owns **users and refresh tokens** and implements the authentication endpoints: register,
log in, refresh, log out, and "who am I". It does not create or check access tokens itself: it
asks the [auth service](libs-auth.md) over gRPC.

- **Path:** [`services/identity/`](../services/identity)
- **Stack:** PHP 8.3 or newer, Laravel 13, Pest for tests
- **Default address:** `127.0.0.1:8000` (`php artisan serve`)
- **Public paths:** reached through the [API gateway](api-gateway.md) as `/v1/auth/*`. Identity itself serves them under `/api/v1/auth/*`; the gateway adds the `/api` prefix.

## Endpoints

| Gateway path | Identity path | Needs token | Purpose |
| --- | --- | --- | --- |
| `POST /v1/auth/register` | `/api/v1/auth/register` | No | Create a user and sign them in |
| `POST /v1/auth/login` | `/api/v1/auth/login` | No | Exchange email and password for tokens |
| `POST /v1/auth/refresh` | `/api/v1/auth/refresh` | No | Exchange a refresh token for a new pair |
| `POST /v1/auth/logout` | `/api/v1/auth/logout` | Yes | Revoke a refresh token |
| `GET /v1/auth/me` | `/api/v1/auth/me` | Yes | The authenticated user |

Every response is JSON. Errors have a `message`; validation errors also have `errors` keyed by field.

### Register

```http
POST /v1/auth/register
Content-Type: application/json

{ "name": "Ada Lovelace", "email": "ada@example.com", "password": "secret-password" }
```

`201 Created`

```json
{ "access_token": "<jwt>", "refresh_token": "<64 random characters>", "token_type": "Bearer", "expires_in": 900 }
```

Rules: `name` required, up to 255 characters; `email` required, valid, up to 255 characters and unique;
`password` required, at least 8 characters. Failures return `422`:

```json
{ "message": "The email has already been taken.", "errors": { "email": ["The email has already been taken."] } }
```

### Log in

```http
POST /v1/auth/login
{ "email": "ada@example.com", "password": "secret-password" }
```

| Status | Meaning |
| --- | --- |
| `200` | The same token pair as register |
| `401` | `{"message":"Invalid credentials."}`, for a wrong password and for an unknown email alike |
| `422` | `email` or `password` missing |
| `429` | Too many failed attempts: `{"message":"Too many failed attempts. Try again later."}` with a `Retry-After` header. See [Login backoff](#login-backoff). |

### Refresh

```http
POST /v1/auth/refresh
{ "refresh_token": "<the refresh token>" }
```

`200` returns a new token pair. The refresh token you sent is revoked at the same time, so each
refresh token works **once**. `401` `{"message":"Invalid refresh token."}` means the token is
unknown, already used, revoked or expired. `422` means `refresh_token` is missing.

### Log out

```http
POST /v1/auth/logout
Authorization: Bearer <access token>
{ "refresh_token": "<the refresh token>" }
```

`204 No Content`. Only a refresh token belonging to the authenticated user is revoked; any other
value is silently ignored. `401` if the access token is missing or invalid, `422` if
`refresh_token` is missing.

### Current user

```http
GET /v1/auth/me
Authorization: Bearer <access token>
```

`200`

```json
{ "id": 42, "name": "Ada Lovelace", "email": "ada@example.com", "email_verified_at": null,
  "created_at": "2026-11-09T12:00:00.000000Z", "updated_at": "2026-11-09T12:00:00.000000Z" }
```

The password and remember token are hidden. `401` if the token is missing, invalid or expired, or
if the user no longer exists.

## Tokens

| | Access token | Refresh token |
| --- | --- | --- |
| Format | RS256 JWT, `sub` is the user id (a UUID) | 64 random characters, opaque |
| Created by | The [auth service](libs-auth.md) (`Issue`) | Identity |
| Lifetime | `AUTH_ACCESS_TTL` on the auth service (default 15 minutes); `expires_in` reports it | `JWT_REFRESH_TTL` minutes on Identity (default 15) |
| Stored | Nowhere (stateless) | Only a SHA-256 hash, in the `refresh_tokens` table |
| Revocable | No, valid until it expires | Yes: on logout, and automatically when used |

Each refresh issues a new refresh token with a fresh lifetime, so `JWT_REFRESH_TTL` acts as an
idle timeout: a client that does not refresh within that time must log in again. With the default of
15 minutes (equal to the access token's), clients must refresh before the access token expires.
Set `JWT_REFRESH_TTL` higher (the design document suggests 30 days, or 43200 minutes) for longer sessions.

A refresh runs in a database transaction that locks the token's row, so two requests using the same
refresh token cannot both succeed.

## Login backoff

Failed logins are slowed down with an exponential lockout, tracked in Redis per **email and client
IP** (case-insensitive email). It is implemented by the
[`LoginBackoff`](../services/identity/app/Http/Middleware/LoginBackoff.php) middleware and applies to
the login route only.

- Each failed login (a `401`) is counted. The first `LOGIN_BACKOFF_FREE_ATTEMPTS` (5) failures are free.
- After that, each further failure locks the account for twice as long as the last, starting at
  `LOGIN_BACKOFF_BASE_SECONDS` (1) and capped at `LOGIN_BACKOFF_MAX_SECONDS` (900).
- While locked, **every** attempt, even with the correct password, gets `429` with `Retry-After`.
  Refused attempts are not counted.
- A successful login clears the count. With no new failure for `LOGIN_BACKOFF_DECAY_SECONDS` (3600),
  Redis forgets it.
- Validation errors (`422`) are not failures and do not count.

With the defaults, the lockout after the Nth failed login is:

| Failure | 1-5 | 6 | 7 | 8 | 9 | 10 | 11 | 12 | 13 | 14 | 15 | 16 and later |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Lockout | none | 1s | 2s | 4s | 8s | 16s | 32s | 64s | 128s | 256s | 512s | 900s |

The lockout end is stored as a millisecond timestamp in a Redis hash, so `Retry-After` is exact.

Identity does **not** throttle register or refresh. The gateway rate limits those per client IP.

## Data model

| Table | Columns |
| --- | --- |
| `users` | `id` (a UUID, generated by the model), `name`, `email` (unique), `email_verified_at`, `password` (hashed), `remember_token`, timestamps |
| `refresh_tokens` | `id`, `user_id` (a UUID; foreign key, cascades on delete), `token_hash` (unique, 64 characters), `expires_at`, `revoked_at` (null until used or revoked), timestamps |

Passwords are hashed by the model's `hashed` cast (bcrypt, `BCRYPT_ROUNDS`). Expired and revoked
refresh tokens stay in the table; there is no cleanup job yet. The Laravel default tables
(sessions, cache, jobs, password resets) also exist but are unused by the auth flow.

## How it is built

```text
routes/api.php                          the five routes; login carries the LoginBackoff middleware
app/Http/Controllers/AuthController.php thin: validate, call AuthService, respond
app/Http/Middleware/LoginBackoff.php    Redis exponential backoff for failed logins
app/Services/AuthService.php            register, login, refresh (rotation), logout
app/Services/TokenService.php           interface: issue(subject), verify(token)
app/Services/GrpcTokenService.php       TokenService backed by the auth service over gRPC
app/Grpc/AuthClient.php                 hand-written gRPC client stub
app/Grpc/Auth/V1, app/Grpc/GPBMetadata  generated protobuf classes (do not edit)
app/Models/User.php, RefreshToken.php   Eloquent models
app/Providers/AppServiceProvider.php    binds TokenService; registers the `jwt` guard
config/auth.php                         the `api` guard (driver `jwt`) and `login_backoff`
config/jwt.php                          refresh lifetime and the auth service address
```

Authentication of protected routes: the `api` guard uses a custom `jwt` driver. It reads the bearer
token, calls `TokenService::verify()` (a gRPC `Verify` call), and loads the user whose id is the
subject. An empty token returns "unauthenticated" without any RPC.

**Fail closed.** If the auth service cannot be reached or answers with anything other than
accepted/rejected, `GrpcTokenService` throws and the request ends in a `500`. A token is never
treated as valid because the check failed. When issuing tokens, the access token is requested
first and the refresh token stored only afterwards, so a failed call leaves no orphaned refresh token.

## Configuration

Set in `.env` (see [`.env.example`](../services/identity/.env.example)).

| Variable | Default | Description |
| --- | --- | --- |
| `JWT_REFRESH_TTL` | `15` | Refresh token lifetime, in minutes |
| `AUTH_RPC_ADDR` | `127.0.0.1:9090` | Address of the [auth service](libs-auth.md) |
| `AUTH_RPC_TIMEOUT` | `2` | Deadline for each auth service call, in seconds |
| `LOGIN_BACKOFF_FREE_ATTEMPTS` | `5` | Failed logins allowed before any lockout |
| `LOGIN_BACKOFF_BASE_SECONDS` | `1` | First lockout length |
| `LOGIN_BACKOFF_MAX_SECONDS` | `900` | Longest lockout |
| `LOGIN_BACKOFF_DECAY_SECONDS` | `3600` | Failures are forgotten after this long without a new one (keep it larger than the maximum lockout) |
| `DB_CONNECTION` and `DB_*` | `sqlite` | The database. The design document calls for PostgreSQL in deployment. |
| `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD`, `REDIS_PREFIX` | `127.0.0.1`, `6379`, none | Redis for login backoff (the `phpredis` client) |

## Run it

Needs PHP 8.3 or newer with the **`redis`** and **`grpc`** extensions, Composer, a running Redis, and
the [auth service](libs-auth.md) reachable at `AUTH_RPC_ADDR`.

```bash
cd services/identity
composer install
cp .env.example .env && php artisan key:generate
touch database/database.sqlite && php artisan migrate
php artisan serve                      # http://127.0.0.1:8000
```

Call it directly under `/api/v1/auth/...`, or put the [gateway](api-gateway.md) in front.

**Upgrading a development database:** user ids changed from integers to UUIDs (the
[wallet service](wallet-service.md) identifies users by UUID), by editing the original migrations. A database
created before that has the old column types, so recreate it with `php artisan migrate:fresh`. **That drops
every table and its data**; nothing is deployed yet, so nothing real is lost.

## Tests

```bash
cd services/identity
php artisan test --compact                    # needs a running Redis
vendor/bin/pint --dirty --format agent        # formatting
```

- The controller tests replace the token service with an in-memory fake, so they need neither the
  `grpc` extension nor the auth service. Signature and expiry behaviour is tested in
  [`libs/auth`](libs-auth.md#testing).
- `GrpcTokenServiceTest` checks how gRPC statuses map to results, against a mocked client.
- The login tests use the **real local Redis** (under the prefix `identity-testing-`), because the
  backoff runs on every login request. Each test uses its own emails, and time is frozen, so lockouts are exact.
- A real PHP-to-Go call is not covered by the test suite; it needs the `grpc` extension and a running auth service.

## Regenerating the protobuf classes

After a change to the [proto contract](../libs/auth/proto/auth/v1/auth.proto), run
`make -C libs/auth proto-php`. See [libs/auth](libs-auth.md#regenerating-code-after-a-contract-change).

## Security notes

- **Behind the gateway.** Identity trusts the network position: it should only be reachable from the gateway. It does not rely on `X-User-ID`; it verifies the token itself, so a direct call with a forged header gets nowhere.
- **Client IP.** Login backoff keys on `$request->ip()`. Behind the gateway or a load balancer, configure Laravel's trusted proxies so that is the real client address; otherwise every user shares the proxy's address.
- **Redis required.** If Redis is down, login returns 500 (fail closed).
- **Unknown email timing.** A login for an unknown email skips password hashing, so it answers faster than one for a real email. The response is identical, but the timing difference is measurable. It is not mitigated yet.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| Every auth endpoint returns 500 with `Class "Grpc\ChannelCredentials" not found` in debug mode | The `grpc` PHP extension is not installed. Install it. Identity builds its gRPC client on every auth request. |
| `500` and `Auth service Issue failed with gRPC status 14` in the log | The auth service is not reachable at `AUTH_RPC_ADDR` (status 14 is UNAVAILABLE) |
| `500` on login, Redis errors in the log | Redis is down or `REDIS_*` is wrong |
| `429` on login even with the right password | The email and IP are locked out. Wait for `Retry-After`, or delete the `login-backoff:*` key in Redis |
| `401` on `me` with a token that worked a minute ago | The access token expired (15 minutes). Refresh it. The user could also have been deleted. |
| `401` on refresh | The refresh token was already used, revoked or expired. Each works once. |
| Tests fail with Redis connection errors | Start Redis; the login tests need it |
| Errors about `user_id`, or logins failing, on a database made before the UUID change | It still has integer ids. `php artisan migrate:fresh` recreates it (and drops its data). |

## Not built yet

From the design document: phone numbers, user status, the `sessions` table, MFA and
OAuth2 clients, and the `auth.events` Kafka events. Also: email verification, password reset
endpoints, cleanup of expired refresh tokens, revoking access tokens before they expire, and a
dedicated API resource for the user (`me` returns the model, with sensitive fields hidden).
