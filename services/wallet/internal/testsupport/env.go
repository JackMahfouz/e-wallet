package testsupport

import (
	"log/slog"
	"testing"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
)

// Env is a Service wired to in-memory fakes, with handles on each of them.
type Env struct {
	Svc     *application.Service
	Store   *MemStore
	Ledger  *Ledger
	Funding *Funding
	Tokens  *Tokens
	Clock   *Clock
	IDs     *SeqIDs
	Options application.Options
}

// NewEnv builds an Env with funding enabled and the default rules.
func NewEnv(t testing.TB, mutate ...func(*application.Options)) *Env {
	t.Helper()

	opts := application.DefaultOptions()
	opts.FundingEnabled = true
	for _, fn := range mutate {
		fn(&opts)
	}

	e := &Env{
		Store: NewMemStore(), Ledger: &Ledger{}, Funding: &Funding{}, Tokens: &Tokens{},
		Clock: NewClock(), IDs: &SeqIDs{}, Options: opts,
	}
	e.Svc = application.New(application.Dependencies{
		Store: e.Store, Ledger: e.Ledger, Tokens: e.Tokens, OTP: OTP{}, Funding: e.Funding,
		Clock: e.Clock, IDs: e.IDs, Logger: slog.New(slog.DiscardHandler),
	}, opts)

	return e
}
