package recovery_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JackMahfouz/e-wallet/services/wallet/internal/adapter/in/recovery"
	"github.com/JackMahfouz/e-wallet/services/wallet/internal/application"
)

type fakeCore struct {
	calls  atomic.Int32
	active atomic.Int32
	max    atomic.Int32
	fail   bool
	block  time.Duration
}

func (f *fakeCore) Recover(ctx context.Context) (application.RecoveryReport, error) {
	f.calls.Add(1)

	n := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		m := f.max.Load()
		if n <= m || f.max.CompareAndSwap(m, n) {
			break
		}
	}

	if f.block > 0 {
		select {
		case <-time.After(f.block):
		case <-ctx.Done():
		}
	}
	if f.fail {
		return application.RecoveryReport{}, errors.New("database down")
	}

	return application.RecoveryReport{Advanced: 1}, nil
}

func run(t *testing.T, core *fakeCore, interval time.Duration) (cancel func(), done <-chan struct{}) {
	t.Helper()

	ctx, stop := context.WithCancel(context.Background())
	finished := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		recovery.New(core, interval, slog.New(slog.DiscardHandler)).Run(ctx)
		close(finished)
	}()
	t.Cleanup(func() { stop(); wg.Wait() })

	return stop, finished
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestItRunsAtOnceAndThenEveryInterval(t *testing.T) {
	core := &fakeCore{}
	run(t, core, 20*time.Millisecond)

	eventually(t, "several passes", func() bool { return core.calls.Load() >= 4 })
}

func TestItStopsWhenTheContextEnds(t *testing.T) {
	core := &fakeCore{}
	stop, done := run(t, core, 10*time.Millisecond)
	eventually(t, "a first pass", func() bool { return core.calls.Load() >= 1 })

	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}

	after := core.calls.Load()
	time.Sleep(60 * time.Millisecond)
	if core.calls.Load() != after {
		t.Error("passes kept running after Run returned")
	}
}

func TestAFailingPassDoesNotStopTheNext(t *testing.T) {
	core := &fakeCore{fail: true}
	run(t, core, 10*time.Millisecond)

	eventually(t, "passes to keep happening despite errors", func() bool { return core.calls.Load() >= 3 })
}

func TestPassesNeverOverlap(t *testing.T) {
	core := &fakeCore{block: 40 * time.Millisecond}
	run(t, core, 5*time.Millisecond) // the interval is shorter than a pass

	eventually(t, "a few slow passes", func() bool { return core.calls.Load() >= 3 })
	if got := core.max.Load(); got != 1 {
		t.Errorf("up to %d passes ran at once, want 1", got)
	}
}

func TestAZeroIntervalMeansItNeverRuns(t *testing.T) {
	core := &fakeCore{}
	_, done := run(t, core, 0)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with no interval should return at once")
	}
	if core.calls.Load() != 0 {
		t.Error("a disabled runner must not run a pass")
	}
}
