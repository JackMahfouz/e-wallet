// Package recovery is the driving adapter that wakes the core on a timer so it can finish what
// stopped part-way: operations waiting on a dependency, and confirmations nobody answered. It owns
// the clock tick and nothing else; what a pass does is the core's business.
package recovery

import (
	"context"
	"log/slog"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
)

// Runner calls Recover every interval until its context ends.
type Runner struct {
	core     application.Recovery
	interval time.Duration
	log      *slog.Logger
}

// New returns a Runner. An interval of zero or less means it does nothing.
func New(core application.Recovery, interval time.Duration, log *slog.Logger) *Runner {
	return &Runner{core: core, interval: interval, log: log}
}

// Run blocks, running a pass at once and then one every interval, until ctx ends. A pass that
// fails is logged and the next one tries again; passes never overlap.
func (r *Runner) Run(ctx context.Context) {
	if r.interval <= 0 {
		return
	}

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		r.pass(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	report, err := r.core.Recover(ctx)
	if err != nil {
		r.log.ErrorContext(ctx, "recovery pass failed", "error", err)

		return
	}

	if report.Advanced > 0 || report.Expired > 0 {
		r.log.InfoContext(ctx, "recovery pass", "advanced", report.Advanced, "expired_confirmations", report.Expired)
	}
}
