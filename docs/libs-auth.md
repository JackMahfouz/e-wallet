# Auth library and service (`libs/auth`)

`libs/auth` is a small Go gRPC server that does one job: it **issues and verifies access tokens**.
It is the only component that holds the signing key. Other services never see the key; they ask
this one whether a token is good.

- **Module:** `github.com/JackMahfouz/e-wallet/libs/auth`
- **Contract:** [`proto/auth/v1/auth.proto`](../libs/auth/proto/auth/v1/auth.proto) (package `auth.v1`, service `Auth`)
- **Default address:** `127.0.0.1:9090`
- **Internal only.** It must never be routed through the API gateway. See [Security notes](#security-notes).

Callers today: the [API gateway](api-gateway.md) (`Verify`), the [Identity service](identity-service.md) (`Issue` and `Verify`) and the [wallet service](wallet-service.md) (`Verify`, on every request: it re-checks the token and ignores the gateway's `X-User-ID`).

## The gRPC API

```proto
service Auth {
  rpc Issue(IssueRequest)   returns (IssueResponse);   // sign a token for a subject
  rpc Verify(VerifyRequest) returns (VerifyResponse);  // check a token
}

message IssueRequest   { string subject = 1; }                          // becomes the `sub` claim
message IssueResponse  { string access_token = 1; int64 expires_in = 2; } // expires_in in seconds
message VerifyRequest  { string access_token = 1; }
message VerifyResponse { string subject = 1; int64 expires_at = 2; }    // expires_at is unix seconds
```

| RPC | Success | Failure |
| --- | --- | --- |
| `Issue` | A signed token and its lifetime in seconds | `INVALID_ARGUMENT` ("subject is required") for an empty subject; `INTERNAL` ("failed to issue token") if signing fails. Signing details are never returned. |
| `Verify` | The token's subject and expiry | `UNAUTHENTICATED` with message `invalid token` or `token expired` |

Callers must treat `UNAUTHENTICATED` as "reject this token" and **any other error as "could not
check"**, which should fail the request, never allow it.

## Access token format

A standard JWT signed with RS256 (RSA, PKCS#1 v1.5, SHA-256), so any JWT tool can inspect it.

```json
// header
{ "alg": "RS256", "typ": "JWT" }
// payload
{ "iss": "e-wallet", "sub": "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "iat": 1794232000, "exp": 1794232900 }
```

`sub` is the user id as a string (a UUID, since Identity's ids became UUIDs; the auth service itself accepts any non-empty subject), `iss` is `AUTH_ISSUER`, and `exp` is `iat` plus `AUTH_ACCESS_TTL`.
The signature is made with the private key in `AUTH_PRIVATE_KEY_FILE`.

`Verify` rejects a token if any of these is true:

- it does not have exactly three dot-separated parts
- the header `alg` is anything other than `RS256` (this shuts out `none` and key-confusion attacks)
- the signature is invalid, or is not in canonical base64url form
- `iss` does not match the configured issuer
- `sub` is empty, or `exp` is missing or not positive
- the current time is at or after `exp`

## Configuration

All settings are environment variables.

| Variable | Default | Description |
| --- | --- | --- |
| `AUTH_PRIVATE_KEY_FILE` | **required** | Path to a PEM RSA private key (PKCS#1 `RSA PRIVATE KEY` or PKCS#8 `PRIVATE KEY`), at least 2048 bits. A file path, not the key itself, so the key can be mounted from a secret store. |
| `AUTH_GRPC_ADDR` | `127.0.0.1:9090` | Listen address. Loopback by default; in a cluster set `0.0.0.0:9090` behind a ClusterIP Service. |
| `AUTH_ISSUER` | `e-wallet` | Value of the `iss` claim, checked on verify. |
| `AUTH_ACCESS_TTL` | `15m` | Access token lifetime, a Go duration such as `90s` or `15m`. Must be positive. |

The server refuses to start on a missing, unreadable, non-PEM, non-RSA or too-small key, or on an invalid TTL.

## Run it

```bash
cd libs/auth
openssl genrsa -out auth.pem 2048          # *.pem is git-ignored; never commit a key
AUTH_PRIVATE_KEY_FILE=auth.pem go run ./cmd/server
```

```text
INFO auth server listening addr=127.0.0.1:9090 issuer=e-wallet access_ttl=15m0s
```

- **Health:** the standard gRPC health service (`grpc.health.v1.Health`) is registered. Both the
  overall server (empty service name) and `auth.v1.Auth` report `SERVING`.
- **Shutdown:** `SIGINT` or `SIGTERM` stops it gracefully and it exits with code 0.
- **Not available:** gRPC reflection is off, and there is no HTTP `/health` or `/metrics` endpoint.

## Use it from another service

**Go** (the generated client is importable because it lives in `gen/`, not `internal/`):

```go
import (
	authv1 "github.com/JackMahfouz/e-wallet/libs/auth/gen/auth/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

conn, err := grpc.NewClient("auth:9090", grpc.WithTransportCredentials(insecure.NewCredentials()))
client := authv1.NewAuthClient(conn)

resp, err := client.Verify(ctx, &authv1.VerifyRequest{AccessToken: token})
switch {
case status.Code(err) == codes.Unauthenticated:
	// the token is bad or expired: reject the request
case err != nil:
	// the auth service could not be reached: fail the request, do not allow it
default:
	userID := resp.GetSubject()
}
```

Until the module is published, point your `go.mod` at the local path, as the gateway does:
`replace github.com/JackMahfouz/e-wallet/libs/auth => ../libs/auth`.

The channel above is plaintext because the port is internal and TLS is expected from the mesh
sidecar. Keep the call inside the trusted network.

**PHP:** Identity uses generated message classes plus a small hand-written client; see
[`GrpcTokenService`](../services/identity/app/Services/GrpcTokenService.php) and
[`AuthClient`](../services/identity/app/Grpc/AuthClient.php). It needs the `grpc` PHP extension.

**Any other language:** generate a client from the `.proto` file.

## Code layout

```text
libs/auth/
  proto/auth/v1/auth.proto        the contract; the single source of truth
  gen/auth/v1/                    generated Go code (committed so other modules can import it)
  cmd/server/main.go              entry point: config, listener, gRPC server, health, shutdown
  internal/config/                environment loading and key parsing
  internal/domain/                claims and the error values (no infrastructure imports)
  internal/service/               RS256 issue and verify, using only the Go standard library
  internal/handler/               maps the gRPC API onto the service and picks the status codes
  Makefile                        proto, proto-go, proto-php, test
```

The layering follows the system design: the handler is the only layer that touches gRPC, the
service knows nothing about transport, and the domain imports no infrastructure.

## Regenerating code after a contract change

Needs `protoc`, `protoc-gen-go` and `protoc-gen-go-grpc` on your `PATH`.

```bash
cd libs/auth
make proto-go      # Go messages and gRPC stubs into gen/
make proto-php     # PHP message classes into services/identity/app/Grpc/
make proto         # both
```

`make proto-php` writes to `../../services/identity/app` by default; override it with
`PHP_OUT=/some/dir`. Generated PHP files are listed in Identity's `pint.json` exclusions so the
formatter does not alter them. Do not edit generated files by hand.

Identity's gRPC client (`AuthClient`) is not generated, so it must be updated by hand when an RPC
is added or renamed.

## Testing

```bash
cd libs/auth && make test      # go vet, then go test -race
```

The tests cover the token service (round trip, expiry boundary, tampered payload, wrong key, `alg`
`none` and HS256 headers, wrong issuer, empty subject, malformed tokens), the handler over a real
loopback gRPC connection (status codes and that internal errors are not leaked), and configuration
loading (defaults, both key formats, every rejection).

## Security notes

- **Internal only.** There is no application-level secret on the port. Rely on network policy or
  the mesh to restrict callers, and never route this server through the gateway.
- **Any caller can mint tokens.** `Issue` is open to every service that can reach the port, not just
  Identity. An Istio `AuthorizationPolicy` allowing only Identity to call `Issue` is the intended follow-up.
- **Key custody.** Keep the PEM file readable only by the auth process. Rotating the key
  invalidates every outstanding token (there is no `kid` or multi-key support), which costs users at
  most one access token lifetime.
- **No early revocation.** Access tokens cannot be revoked before they expire. Logout revokes only
  the refresh token (see [Identity](identity-service.md)).
- **No Vault integration.** The key is read from a file at startup; wiring the file to Vault is a
  deployment task.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Server exits at start with `AUTH_PRIVATE_KEY_FILE is required` | The variable is not set |
| `private key ... need at least 2048` | Generate a larger key: `openssl genrsa -out auth.pem 2048` |
| `private key ... not an RSA key` | The PEM holds an EC or other key type |
| `listen on 127.0.0.1:9090: address already in use` | Another process owns the port; change `AUTH_GRPC_ADDR` |
| Callers get `UNAUTHENTICATED` with `token expired` | The access token is older than `AUTH_ACCESS_TTL`; refresh it through Identity |
| Every token is suddenly `invalid token` | The server restarted with a different key or `AUTH_ISSUER` |
