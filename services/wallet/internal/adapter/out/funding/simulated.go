// Package funding is the driven adapter for moving money between an account and the outside world.
//
// Simulated succeeds immediately and moves nothing real: a "deposit" creates ledger money with
// nothing behind it. It is off unless the wallet is started with WALLET_FUNDING_MODE=simulated, and
// is a stand-in for the Payment service's card and bank rails.
package funding

import (
	"context"
	"fmt"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/domain"
)

// Simulated is a funding gateway that always says yes. Both calls are trivially idempotent.
type Simulated struct{}

var _ application.FundingGateway = Simulated{}

// Collect implements application.FundingGateway.
func (Simulated) Collect(context.Context, string, int64) error { return nil }

// Payout implements application.FundingGateway.
func (Simulated) Payout(context.Context, string, int64) error { return nil }

// Refusing is the gateway used when funding is switched off: it refuses everything, for good. The
// core already turns deposits and withdrawals away before it gets here, so this is a second lock,
// not the first: a bug in the first must not reach a gateway that says yes.
type Refusing struct{}

var _ application.FundingGateway = Refusing{}

// Collect implements application.FundingGateway.
func (Refusing) Collect(context.Context, string, int64) error { return errRefused }

// Payout implements application.FundingGateway.
func (Refusing) Payout(context.Context, string, int64) error { return errRefused }

var errRefused = fmt.Errorf("%w: funding is switched off", domain.ErrRejected)
