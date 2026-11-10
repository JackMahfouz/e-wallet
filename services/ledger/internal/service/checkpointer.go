package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/JackMahfouz/e-wallet/services/ledger/internal/domain"
)

// Checkpointer appends checkpoints for every finished month, quarter, year and five-year period
// in which an account had entries.
//
// A checkpoint is the account's closing balance for a period. The current balance is the latest
// checkpoint plus the entries since, so checkpoints keep a balance cheap to compute however long
// the account's history grows. A balance is correct without them; they only make it fast.
type Checkpointer struct {
	repo  domain.Repository
	grace time.Duration
	now   func() time.Time
	log   *slog.Logger
}

// NewCheckpointer returns a Checkpointer. A period is checkpointed once it ended at least grace
// ago. Pass time.Now outside of tests.
func NewCheckpointer(repo domain.Repository, grace time.Duration, now func() time.Time, log *slog.Logger) *Checkpointer {
	return &Checkpointer{repo: repo, grace: grace, now: now, log: log}
}

// RunOnce appends every checkpoint that is due and returns how many it wrote.
//
// One account failing does not stop the others: errors are logged, collected, and returned
// together at the end. Running it again is harmless, since a period that has a checkpoint is
// never given another.
func (c *Checkpointer) RunOnce(ctx context.Context) (int, error) {
	closedBefore := c.now().Add(-c.grace)

	var (
		written int
		errs    []error
	)

	for _, period := range domain.AllPeriods {
		accounts, err := c.repo.AccountsPendingCheckpoint(ctx, period)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		for _, accountID := range accounts {
			// An account may owe several periods (after downtime, say): close them oldest first.
			for {
				if err := ctx.Err(); err != nil {
					return written, errors.Join(append(errs, err)...)
				}

				appended, err := c.repo.AppendNextCheckpoint(ctx, accountID, period, closedBefore)
				if err != nil {
					c.log.Error("checkpoint failed", "account_id", accountID, "period", period, "error", err)
					errs = append(errs, err)

					break
				}
				if !appended {
					break
				}

				written++
			}
		}
	}

	return written, errors.Join(errs...)
}

// Run calls RunOnce immediately and then every interval until ctx is cancelled.
func (c *Checkpointer) Run(ctx context.Context, interval time.Duration) {
	run := func() {
		written, err := c.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			c.log.Error("checkpoint run finished with errors", "written", written, "error", err)

			return
		}
		if written > 0 {
			c.log.Info("checkpoints appended", "count", written)
		}
	}

	run()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
