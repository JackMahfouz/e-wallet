# Shared money package (`libs/process-money`)

A small Go package that is the **one place money is cast** between the decimal numbers people and
callers speak (`12.34`) and the whole cents every service stores and sums. The [ledger](ledger-service.md)
and the [wallet](wallet-service.md) both import it, so the rules for what counts as a valid amount are
written once, tested once, and cannot drift apart.

- **Path:** [`libs/process-money/`](../libs/process-money), module `github.com/JackMahfouz/e-wallet/libs/process-money`, package `processmoney`
- **No dependencies and no I/O.** It is a library, not a service: there is no network hop, port or deployment. A service imports it and wires it in with a `replace` directive, the way it does `libs/auth`.
- **Foreign exchange** will be added here later. There is nothing for it yet.

## The API

```go
import processmoney "github.com/JackMahfouz/e-wallet/libs/process-money"

cents, err := processmoney.ToMinorUnits(19.99)      // 1999, nil
amount     := processmoney.FromMinorUnits(1999)     // 19.99
balance, err := processmoney.BalanceToMinorUnits(0) // 0, nil: a balance may be zero or negative
```

| Name | What it does |
| --- | --- |
| `ToMinorUnits(amount float64) (int64, error)` | Casts an amount to whole cents. For amounts people send. Refuses anything that is not a valid amount. |
| `BalanceToMinorUnits(balance float64) (int64, error)` | Casts a balance reported by another service back to cents. Like `ToMinorUnits`, but accepts zero and negative values. |
| `FromMinorUnits(cents int64) float64` | Cents back to a decimal, for reading. |
| `MaxAmount` | `10000000000`: the largest single amount. |
| `MaxBalance` | `20000000000`: the largest balance magnitude `BalanceToMinorUnits` accepts. |
| `ErrInvalidAmount` | Wrapped by every validation failure, so callers can map it to a bad-request status with `errors.Is`. |

## What counts as a valid amount

An amount is accepted when it is **finite, greater than zero, at most `MaxAmount`, and a whole number of
cents**. It is never silently rounded to a different value.

| Given | Result |
| --- | --- |
| `19.99` | 1999 cents |
| `0.1 + 0.2` (which is `0.30000000000000004`) | 30 cents: float noise is tolerated |
| `1.15` (which is `114.99999999999999` cents) | 115 cents |
| `10.005` | Refused: `must have at most 2 decimal places` |
| `0`, `-5` | Refused: `must be greater than 0` |
| `0.00001` | Refused: `must be at least 0.01` |
| `NaN`, infinity | Refused: `must be a finite number` |
| `10000000000.01` | Refused: `must not exceed 10000000000` |

"Noise" means within a thousandth of a cent of a whole cent. The `MaxAmount` limit is what makes that safe:
at that size a float still holds every whole cent exactly enough to be told apart from a fractional one.
`BalanceToMinorUnits` applies the same whole-cent rule, and `MaxBalance` (twice `MaxAmount`) is the largest
size at which that still holds.

## Why floats at the edge and integers inside

The APIs take and return decimals because that is what callers want to send. But binary floats cannot hold
`0.1` exactly, so adding them drifts. The package is the one boundary: past `ToMinorUnits` nothing does
float arithmetic on money, every sum is exact integer arithmetic, and `FromMinorUnits` is the only way back.
Every value a service hands out can be sent back in and means exactly the same cents; a property test
checks that for every whole-cent amount up to 3,000.00 and half a million random ones beyond it.

## Who uses it

| Service | Where |
| --- | --- |
| Ledger | Its gRPC handler casts every amount on the way in and out. It used to carry its own copy; the code and its tests moved here unchanged. |
| Wallet | The REST and gRPC adapters cast amounts at the edge; the ledger client casts cents to decimals and balances back. The wallet's core only ever sees cents. |

## Tests

```bash
cd libs/process-money && make test
```

The tests cover the table above, the rejections and their messages, the exact decimal for every cent
value, and the round trip of 300,000 consecutive and 500,000 random cent values (and the same for balances
across the range from minus to plus `MaxBalance`). Mutation checks (widen the tolerance, drop the maximum,
round down instead of to nearest, accept `NaN`, and others) were run against it and every one is caught.
