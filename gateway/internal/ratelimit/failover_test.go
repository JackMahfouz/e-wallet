package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const retryInterval = 5 * time.Second

type fakeLimiter struct {
	mu     sync.Mutex
	calls  int
	err    error
	result Result
}

func (f *fakeLimiter) Allow(context.Context, string, int, time.Duration) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	return f.result, f.err
}

func (f *fakeLimiter) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeLimiter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func newFailover(c *clock, primary, fallback Limiter) (*Failover, *bytes.Buffer) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	return NewFailover(primary, fallback, retryInterval, c.Now, logger), &logs
}

func TestFailoverUsesThePrimaryWhileItWorks(t *testing.T) {
	primary := &fakeLimiter{result: Result{Allowed: false, RetryAfter: 7 * time.Second}}
	fallback := &fakeLimiter{result: Result{Allowed: true}}
	f, _ := newFailover(newClock(), primary, fallback)

	result := mustAllow(t, f, "k", 1, time.Minute)

	if result.Allowed || result.RetryAfter != 7*time.Second {
		t.Errorf("result = %+v, want the primary's answer", result)
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback called %d times, want 0", fallback.callCount())
	}
}

func TestFailoverFallsBackWhenThePrimaryFailsAndLogsOnce(t *testing.T) {
	primary := &fakeLimiter{err: errors.New("connection refused")}
	fallback := &fakeLimiter{result: Result{Allowed: true}}
	f, logs := newFailover(newClock(), primary, fallback)

	for i := range 5 {
		if result := mustAllow(t, f, "k", 1, time.Minute); !result.Allowed {
			t.Fatalf("request %d: %+v, want the fallback's answer", i, result)
		}
	}

	if got := strings.Count(logs.String(), "unavailable"); got != 1 {
		t.Errorf("outage logged %d times, want once:\n%s", got, logs.String())
	}
	if fallback.callCount() != 5 {
		t.Errorf("fallback called %d times, want 5", fallback.callCount())
	}
}

func TestFailoverProbesThePrimaryOncePerRetryInterval(t *testing.T) {
	c := newClock()
	primary := &fakeLimiter{err: errors.New("down")}
	f, _ := newFailover(c, primary, &fakeLimiter{result: Result{Allowed: true}})

	for range 10 {
		mustAllow(t, f, "k", 1, time.Minute)
	}
	if got := primary.callCount(); got != 1 {
		t.Fatalf("primary called %d times in one interval, want 1 (not once per request)", got)
	}

	c.Advance(retryInterval - time.Second)
	mustAllow(t, f, "k", 1, time.Minute)
	if got := primary.callCount(); got != 1 {
		t.Fatalf("primary called %d times before the interval ended, want 1", got)
	}

	c.Advance(time.Second)
	for range 10 {
		mustAllow(t, f, "k", 1, time.Minute)
	}
	if got := primary.callCount(); got != 2 {
		t.Errorf("primary called %d times after the interval, want exactly 1 more probe (2 total)", got)
	}
}

// gatedLimiter fails its first call, then blocks every later call until release is closed.
type gatedLimiter struct {
	calls   atomic.Int32
	release chan struct{}
}

func (g *gatedLimiter) Allow(context.Context, string, int, time.Duration) (Result, error) {
	if g.calls.Add(1) == 1 {
		return Result{}, errors.New("down")
	}
	<-g.release

	return Result{Allowed: false}, nil
}

func TestFailoverSendsOnlyOneProbeWhileAnotherIsStillInFlight(t *testing.T) {
	c := newClock()
	primary := &gatedLimiter{release: make(chan struct{})}
	f, _ := newFailover(c, primary, &fakeLimiter{result: Result{Allowed: true}})
	mustAllow(t, f, "k", 1, time.Minute)
	c.Advance(retryInterval)

	results := make(chan Result, 10)
	for range 10 {
		go func() {
			result, _ := f.Allow(context.Background(), "k", 1, time.Minute)
			results <- result
		}()
	}

	// One request is the probe and is stuck waiting on Redis. The other nine must not pile onto
	// a struggling Redis: the fallback answers them straight away.
	for i := range 9 {
		select {
		case result := <-results:
			if !result.Allowed {
				t.Fatalf("a request got the primary's answer %+v, want the fallback's", result)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 9 requests were answered by the fallback while a probe was in flight", i)
		}
	}
	if got := primary.calls.Load(); got != 2 {
		t.Errorf("primary called %d times, want 2 (the failure and the single in-flight probe)", got)
	}

	close(primary.release)
	<-results
}

func TestFailoverReturnsToThePrimaryOnceAProbeSucceeds(t *testing.T) {
	c := newClock()
	primary := &fakeLimiter{err: errors.New("down"), result: Result{Allowed: false, RetryAfter: time.Second}}
	fallback := &fakeLimiter{result: Result{Allowed: true}}
	f, logs := newFailover(c, primary, fallback)
	mustAllow(t, f, "k", 1, time.Minute)

	primary.setError(nil)
	c.Advance(retryInterval)
	probe := mustAllow(t, f, "k", 1, time.Minute)
	next := mustAllow(t, f, "k", 1, time.Minute)

	if probe.Allowed || next.Allowed {
		t.Errorf("probe = %+v, next = %+v; want both to be the primary's answer (blocked)", probe, next)
	}
	if got := strings.Count(logs.String(), "recovered"); got != 1 {
		t.Errorf("recovery logged %d times, want once:\n%s", got, logs.String())
	}
	if primary.callCount() != 3 {
		t.Errorf("primary called %d times, want 3 (failure, probe, next)", primary.callCount())
	}
}

func TestFailoverDoesNotTreatACancelledRequestAsAnOutage(t *testing.T) {
	primary := &fakeLimiter{err: context.Canceled}
	fallback := &fakeLimiter{result: Result{Allowed: true}}
	f, logs := newFailover(newClock(), primary, fallback)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.Allow(ctx, "k", 1, time.Minute); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	primary.setError(nil)
	mustAllow(t, f, "k", 1, time.Minute)

	if primary.callCount() != 2 {
		t.Errorf("primary called %d times, want 2: a cancelled request must not switch to the fallback", primary.callCount())
	}
	if strings.Contains(logs.String(), "unavailable") {
		t.Errorf("outage logged for a cancelled request:\n%s", logs.String())
	}
}

func TestFailoverKeepsLimitingWithRealRedisDownThenReturnsToIt(t *testing.T) {
	c := newClock()
	limiter, mr := newRedisLimiter(t)
	f, _ := newFailover(c, limiter, NewMemory(c.Now))

	if !mustAllow(t, f, "k", 2, time.Minute).Allowed {
		t.Fatal("request through Redis blocked, want allowed")
	}
	if !mr.Exists("test:k") {
		t.Fatal("counter not written to Redis while it was up")
	}

	mr.Close()
	allowed := 0
	for range 5 {
		if mustAllow(t, f, "k", 2, time.Minute).Allowed {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("allowed %d requests with Redis down, want 2: the in-memory limiter must enforce the same limit", allowed)
	}

	if err := mr.Restart(); err != nil {
		t.Fatalf("restart redis: %v", err)
	}
	mr.FlushAll()
	c.Advance(retryInterval)
	mustAllow(t, f, "k", 2, time.Minute)

	if !mr.Exists("test:k") {
		t.Error("counter not written to Redis after it came back: the limiter did not switch back")
	}
}
