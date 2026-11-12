# Ledger service

The ledger is an **append-only record of debits and credits**, one ledger per account. Every
post adds a new entry; nothing is ever updated or deleted. Other services (the [wallet service](wallet-service.md) and, later, payment) post to it and read from it over gRPC.

- **Path:** [`services/ledger/`](../services/ledger), module `github.com/JackMahfouz/e-wallet/services/ledger`
- **Stack:** Go, gRPC, PostgreSQL (13 or newer; tested on 16)
- **Default address:** `127.0.0.1:9091` (gRPC)
- **Contract:** [`proto/ledger/v1/ledger.proto`](../services/ledger/proto/ledger/v1/ledger.proto) (package `ledger.v1`, service `Ledger`)
- **Internal only.** It must never be routed through the [API gateway](api-gateway.md). See [Security notes](#security-notes).

The ledger does not own accounts. Accounts (and which user has which) belong to the [wallet service](wallet-service.md); the ledger only knows an account's id (a UUID). The first credit to an unknown id starts that
account's ledger.

## The gRPC API

```proto
service Ledger {
  rpc Debit(PostRequest)  returns (PostResponse);   // append a debit entry
  rpc Credit(PostRequest) returns (PostResponse);   // append a credit entry

  rpc GetBalance(GetBalanceRequest)         returns (GetBalanceResponse);
  rpc ListEntries(ListEntriesRequest)       returns (ListEntriesResponse);
  rpc ListCheckpoints(ListCheckpointsRequest) returns (ListCheckpointsResponse);
}
```

There is no update or delete call, by design (a test fails if one is ever added). Corrections are made
by appending a new entry.

### Posting: `Debit` and `Credit`

```proto
message PostRequest {
  string account_id     = 1;  // UUID of the account
  string transaction_id = 2;  // UUID of the business transaction
  double amount         = 3;  // > 0, at most 2 decimals
}

message PostResponse {
  Entry  entry    = 1;
  double balance  = 2;  // the account's balance after this call
  bool   replayed = 3;  // true when nothing new was appended (see Retries)
}
```

| Result | Code | When |
| --- | --- | --- |
| Entry appended | `OK` | The post was valid and, for a debit, covered by the balance |
| Bad input | `INVALID_ARGUMENT` | `account_id` or `transaction_id` is not a UUID, or the amount is not valid (see [Amounts](#amounts)) |
| Insufficient funds | `FAILED_PRECONDITION` | A debit is larger than the balance. The message states both, for example `insufficient funds: balance 5.00, requested 10.50`. |
| Conflicting retry | `ALREADY_EXISTS` | The `transaction_id` was already posted in this direction with a different amount |
| Slow or abandoned call | `DEADLINE_EXCEEDED`, `CANCELED` | The caller's deadline passed or it cancelled |
| Anything else | `INTERNAL` | Message is just `internal error`; the detail is only in the server log |

**The debit rule.** A debit is refused if its amount is more than the account's balance. An account
with a balance of zero, or with no entries at all, therefore refuses every debit. A debit equal to the
whole balance is allowed and leaves zero.

**Retries are safe.** An entry is identified by its account, `transaction_id` and direction. Posting
the same `transaction_id` to the same account in the same direction again returns the **original
entry** with `replayed = true` and appends nothing, even if the balance has changed since (including
down to zero). Reusing it with a different amount is `ALREADY_EXISTS`. So a caller that times out can
simply retry with the same ids.

### Reading

| Call | Returns |
| --- | --- |
| `GetBalance(account_id)` | `balance` (0 for an account with no entries) and `last_seq`, the sequence number of the newest entry |
| `ListEntries(account_id, after_seq, limit)` | Entries with `seq` above `after_seq`, oldest first, and `has_more`. Start with `after_seq = 0`; continue from the last `seq` returned. `limit` defaults to 100, at most 1000. |
| `ListCheckpoints(account_id, period, limit)` | The account's checkpoints, newest first. An unspecified `period` returns all of them. `limit` as above. |

An entry has an `id`, `account_id`, `transaction_id`, `seq`, `direction`, `amount` and
`created_at_micros`. `seq` is 1, 2, 3, ... per account with no gaps: it is the ledger's true order.
Timestamps are microseconds since the Unix epoch (UTC).

## Amounts

On the API an amount is a plain `double`. Underneath, every amount is stored and summed as **whole
cents in a 64-bit integer**: multiplied by 100 on the way in, divided by 100 on the way out. Floats exist
only at the gRPC edge, so all arithmetic on money is exact integer arithmetic. The casting itself is done by the
shared [money package](process-money.md), which the wallet service uses too, so the two can never disagree about
what a valid amount is.

An amount is accepted when it is finite, greater than zero, at most `10000000000`, and a whole number
of cents. It is never silently rounded to a different value:

| Posted | Result |
| --- | --- |
| `19.99` | Stored as 1999 cents |
| `0.1 + 0.2` (which is `0.30000000000000004`) | Accepted as 30 cents: float noise is tolerated |
| `10.005` | Refused: `must have at most 2 decimal places` |
| `0`, `-5` | Refused: `must be greater than 0` |
| `NaN`, infinity | Refused: `must be a finite number` |
| `10000000000.01` | Refused: `must not exceed 10000000000` |

Reads divide the stored cents by 100, so every value the ledger returns can be posted back and
means exactly the same cents. There is no currency: the ledger assumes one two-decimal currency.

## How the balance works

An account's balance is **its latest checkpoint plus every entry after it**. There is no stored
running balance and nothing to update. With no checkpoint yet it is simply the sum of all the
account's entries, so a balance is always correct and checkpoints only make it fast.

### Checkpoints

A checkpoint records an account's closing balance for a finished period:

| Period | Covers |
| --- | --- |
| `month` | A calendar month |
| `quarter` | Jan to Mar, Apr to Jun, Jul to Sep, or Oct to Dec |
| `year` | A calendar year |
| `five_year` | A block of five calendar years aligned to multiples of five: 2025 to 2029, 2030 to 2034, ... |

Periods are half-open and in UTC. An entry stamped exactly at the end of a period belongs to the next one.
A checkpoint holds the period's start and end, `last_seq` (the newest entry it covers) and `balance`
(the balance after that entry).

- **When.** A background job inside the server looks for finished periods at startup and then every
  `LEDGER_CHECKPOINT_INTERVAL` (default one hour). A period is checkpointed once it ended at least
  `LEDGER_CHECKPOINT_GRACE` (default one minute) ago. An account that owes several periods (after
  downtime, say) gets them oldest first.
- **Only periods with activity.** A quiet month gets no checkpoint; the next one simply covers everything since the last.
- **Four independent chains.** Each period type builds on its own previous checkpoint plus the entries
  since, never on a finer period's. The four chains are four separate calculations of the same
  balances, so a mistake in one cannot hide in another. A checkpoint always equals the sum of the
  entries up to its `last_seq`.
- **Safe to run anywhere.** Checkpoints are written under the same per-account lock as posts, so several
  ledger servers can run the job at once without duplicates.
- **Never negative.** The database refuses a checkpoint with a negative balance, which would mean an
  overdraft had slipped through.

Checkpoints are trusted: the balance starts from the latest one. They are produced by the ledger
itself from the entries and cannot be changed afterwards.

### Concurrency

Every post runs in one database transaction that first takes a lock on the account's ledger
(a PostgreSQL advisory lock keyed on the account id). Inside it, the service checks for a retry, reads
the balance, applies the debit rule and appends the entry. Because the balance is read under the lock,
**two concurrent debits can never both be approved against the same money**, even across several ledger servers.
Different accounts never wait for each other.

A transfer between two accounts is two posts (a debit on one account and a credit on the other)
that share a `transaction_id`, so their debits and credits balance per transaction. There is no
atomic transfer call. If the debit succeeds and the credit cannot be completed, retry the credit
with the same `transaction_id` (a retry cannot double-apply), or compensate by crediting the sender back.

## Data model

Two tables, created by [`migrations/001_create_ledger.sql`](../services/ledger/migrations/001_create_ledger.sql).

| `ledger_entries` | |
| --- | --- |
| `id` | UUID, generated |
| `account_id`, `transaction_id` | UUIDs (soft references: no foreign keys, as the design requires across services) |
| `seq` | Per-account sequence, from 1. Unique with `account_id`, so the ledger cannot fork |
| `direction` | `debit` or `credit` |
| `amount` | Cents, a `bigint`, always greater than zero |
| `created_at` | Set by the database with `clock_timestamp()` at insert time, under the account lock, so it never runs backwards against `seq` |

`(account_id, transaction_id, direction)` is unique: that is what makes a retry idempotent.

| `ledger_checkpoints` | |
| --- | --- |
| `id`, `account_id` | As above |
| `period` | `month`, `quarter`, `year` or `five_year` |
| `period_start`, `period_end` | The half-open period; `period_end` is exclusive |
| `last_seq` | The newest entry the checkpoint covers |
| `balance` | Cents, the balance after `last_seq`; never negative |

Unique on `(account_id, period, period_start)`.

### Append-only is enforced by the database

Triggers make PostgreSQL refuse `UPDATE`, `DELETE` and `TRUNCATE` on both tables, even from the table
owner, with SQLSTATE `23001` (`restrict_violation`): `UPDATE on ledger_entries is not allowed: the
ledger is append-only`. Check constraints also refuse a non-positive amount, an unknown direction or
period, and a checkpoint that ends before it starts.

The service only ever runs `INSERT` and `SELECT`. As defence in depth, give its database role only
those two privileges. A superuser can still drop the triggers, so treat database access as part of the
trust boundary.

### Migrations

SQL files in `migrations/` are embedded in the binary and applied at startup with
[goose](https://github.com/pressly/goose) (when `LEDGER_MIGRATE` is true, the default). goose takes a database lock,
so several servers can start at the same moment and only one applies them. The ledger's tables are
never dropped by a migration: the "down" step does nothing.

## Configuration

Environment variables.

| Variable | Default | Description |
| --- | --- | --- |
| `LEDGER_DATABASE_URL` | **required** | PostgreSQL connection string, as a URL or `key=value` form. Pool size can be set in it, for example `?pool_max_conns=20`. |
| `LEDGER_DATABASE_PASSWORD_FILE` | empty | Path to a file holding the database password. It overrides any password in the URL, so the secret need not sit in the environment. |
| `LEDGER_GRPC_ADDR` | `127.0.0.1:9091` | Listen address. Loopback by default; in a cluster use `0.0.0.0:9091` behind a ClusterIP Service. |
| `LEDGER_MIGRATE` | `true` | Apply pending migrations at startup |
| `LEDGER_CHECKPOINT_INTERVAL` | `1h` | How often the background job looks for finished periods. `0` turns it off (balances stay correct but sum more entries). |
| `LEDGER_CHECKPOINT_GRACE` | `1m` | How long after a period ends before it is checkpointed |

The server refuses to start on a missing database URL, an unparsable setting, or if the database cannot be reached.

## Run it

You need a PostgreSQL. A throwaway one in Docker:

```bash
cd services/ledger
make test-db-up        # PostgreSQL 16 on 127.0.0.1:55432, user postgres, password ledger, database ledger
LEDGER_DATABASE_URL='postgres://postgres:ledger@127.0.0.1:55432/ledger?sslmode=disable' go run ./cmd/server
```

```text
{"time":"2026-11-10T11:03:16+02:00","level":"INFO","msg":"ledger listening","addr":"127.0.0.1:9091","checkpoint_interval":"1h0m0s","migrations":true}
```

- **Health:** the standard gRPC health service is registered. Both the overall server (empty service name) and `ledger.v1.Ledger` report `SERVING`.
- **Logs:** JSON on standard output. One `rpc` line per call with the method, result code and duration; request contents are not logged. Unexpected errors are logged in full, but callers only see `internal error`.
- **Shutdown:** `SIGINT` or `SIGTERM` stops gracefully: it stops taking calls, lets the checkpoint job finish its step, and exits with code 0.
- **Not available:** gRPC reflection, an HTTP endpoint, and metrics.

## Use it from another service

The generated Go client is importable (it lives in `gen/`, not `internal/`). Until the module is
published, point your `go.mod` at the local path:
`replace github.com/JackMahfouz/e-wallet/services/ledger => ../ledger`. The ledger module depends on
[`libs/process-money`](process-money.md), and a `replace` in a dependency is ignored, so add that one too:
`replace github.com/JackMahfouz/e-wallet/libs/process-money => ../../libs/process-money` (adjust the path to where
your module sits). The [wallet service](wallet-service.md) is the working example.

```go
import (
	ledgerv1 "github.com/JackMahfouz/e-wallet/services/ledger/gen/ledger/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

conn, err := grpc.NewClient("ledger:9091", grpc.WithTransportCredentials(insecure.NewCredentials()))
client := ledgerv1.NewLedgerClient(conn)

// Keep the transaction id: reuse it if you have to retry.
transactionID := uuid.NewString()
response, err := client.Debit(ctx, &ledgerv1.PostRequest{
	AccountId:     accountID,
	TransactionId: transactionID,
	Amount:        19.99,
})
switch status.Code(err) {
case codes.OK:
	// response.Balance is the balance after the debit; response.Replayed is true for a retry
case codes.FailedPrecondition:
	// not enough money: the message says how much there is
case codes.InvalidArgument, codes.AlreadyExists:
	// a bug in the caller: a bad id or amount, or a reused transaction id with another amount
default:
	// the outcome is unknown: retry with the SAME transactionID
}
```

The channel is plaintext because the port is internal and TLS is expected from the mesh sidecar.

## Code layout

```text
services/ledger/
  proto/ledger/v1/ledger.proto   the contract: the single source of truth
  gen/ledger/v1/                 generated Go code (committed so other modules can import it)
  migrations/                    SQL migrations (goose), embedded in the binary
  cmd/server/main.go             entry point: config, database, migrations, gRPC server, checkpoint job
  internal/config/               environment loading
  internal/domain/               entries, periods and the storage interface (no infrastructure imports). Money casting is in libs/process-money
  internal/service/              the rules: debit, credit, retries, balance check; the checkpointer
  internal/store/                the PostgreSQL implementation: queries, locking, checkpoints, migrations
  internal/handler/              gRPC: float <-> cents conversion, status codes, request logging
  internal/testdb/               test helper: a fresh, migrated schema on a real PostgreSQL
  internal/events/               reserved for Kafka producers and consumers; empty
  Makefile                       proto, test, test-db-up, test-db-down
```

The layering follows the system design: `domain` imports no infrastructure, `service` depends only on
`domain` and an interface for storage, and `handler` is the only layer that touches gRPC.

After changing the contract, regenerate the code with `make proto` (needs `protoc`, `protoc-gen-go` and
`protoc-gen-go-grpc` on your `PATH`). Timestamps in the contract are plain `int64` microseconds
because the protoc install on the development machine ships without the standard Timestamp type.

## Testing

The ledger's guarantees (the triggers, the constraints, the lock) live in the database, so the tests run
against a **real PostgreSQL**:

```bash
cd services/ledger
make test-db-up        # start a throwaway PostgreSQL in Docker
make test              # go vet, then go test -race with the database required
make test-db-down
```

Any PostgreSQL works: set `LEDGER_TEST_DATABASE_URL`. Each test gets its own freshly migrated
schema, dropped afterwards, so tests are isolated and run in parallel. Without
`LEDGER_TEST_DATABASE_URL` the database tests are **skipped**; `make test` sets `LEDGER_REQUIRE_TEST_DB`
so a missing database is a failure instead.

What is covered:

- **Money:** the ledger's own tests check that the handler casts amounts at the edge and refuses every kind of invalid amount; the round-trip property test (every whole-cent amount survives the trip through a float) now lives with the [money package](process-money.md).
- **Append-only:** `UPDATE`, `DELETE` and `TRUNCATE` are refused on both tables, and the rows are unchanged afterwards. The database also rejects every row that breaks a constraint.
- **The debit rule:** refused on a zero balance, an unknown account, and an amount one cent over; allowed for exactly the balance.
- **Retries and conflicts**, and that the two legs of a transfer balance per transaction.
- **Concurrency:** 100 parallel debits against 30.00 yield exactly 30 successes, and a random mix of parallel credits and debits never makes the balance negative at any point in the replayed ledger.
- **Checkpoints:** the bounds of every period (leap years, December, five-year alignment, non-UTC input); closing finished periods only; skipping quiet ones; oldest first; boundary entries; out-of-order timestamps; two checkpointers at once; and a long random history in which every balance equals a plain sum of the entries after every step.
- **The gRPC API:** floats on the wire and cents underneath, status codes, paging, and two-way concurrency over a real connection.

## Security notes

- **Internal only.** There is no authentication or authorisation on the port, in line with the [auth service](libs-auth.md). Keep it off the gateway and restrict callers with the mesh or network policy.
- **Any caller can post.** Every service that can reach the port can credit and debit any account. An Istio `AuthorizationPolicy` allowing only the wallet and payment services to post is the intended follow-up; reads could be allowed more widely.
- **The ledger trusts its callers' account ids.** It does not check that an account exists or who owns it; that is the [wallet service](wallet-service.md)'s job.
- **No backdating.** The API cannot set an entry's time: `created_at` is the database's clock when the entry is appended.
- **Database access is part of the trust boundary.** Use a role with only `INSERT` and `SELECT`, and a password file.

## Limitations and not built

- **Accounts and ownership** (users with several accounts) belong to the [wallet service](wallet-service.md).
- **No currency.** One implied two-decimal currency; the design's `currency` column is not there.
- **No atomic transfer call:** a transfer is two posts, as described above.
- **No Kafka.** The design's `ledger.entries` topic is not used; entries are posted over gRPC. [`internal/events/`](../services/ledger/internal/events) is a placeholder.
- **No runtime reconciliation.** Checkpoints are trusted when computing a balance. Nothing yet re-sums an account's history to compare it with its checkpoints.
- **The checkpoint job scans the whole entries table** once per period type per run. That is fine for now; if it ever is not, track the accounts that have new entries instead.
- **The ledger is not routed by the gateway**, deliberately.
- **No Dockerfile or Helm chart**, no metrics, and no HTTP `/health`: only the gRPC health service.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| Exits at start with `LEDGER_DATABASE_URL is required` | The variable is not set |
| Exits with `reach database` | PostgreSQL is not reachable at that URL, or the credentials are wrong |
| Exits with `apply migrations` | The database role cannot create tables or triggers, or another migration is running |
| `FAILED_PRECONDITION: insufficient funds` | The balance is lower than the debit. It is also what you get for any debit on an account that was never credited. |
| `ALREADY_EXISTS` | A `transaction_id` was reused in the same direction on the same account with a different amount |
| `INVALID_ARGUMENT: ... at most 2 decimal places` | The amount is not a whole number of cents. Round it before posting. |
| `INTERNAL: internal error` | See the server log for the real error, usually the database |
| Database tests are skipped | `LEDGER_TEST_DATABASE_URL` is not set. Run `make test-db-up && make test`. |
| `UPDATE on ledger_entries is not allowed` | Working as intended. Append a correcting entry instead. |
