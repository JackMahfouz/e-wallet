package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Failover uses a primary Limiter (Redis) while it works and a fallback (in-memory) while it
// does not.
//
// On a primary error it switches to the fallback and logs once. While the primary is down it
// is probed at most once per retryInterval, so an outage does not add a Redis timeout to every
// request. When a probe succeeds it switches back and logs once. Counters are not carried
// across a switch, so limits are briefly looser around it.
type Failover struct {
	primary       Limiter
	fallback      Limiter
	retryInterval time.Duration
	now           func() time.Time
	log           *slog.Logger

	mu        sync.Mutex
	down      bool
	nextProbe time.Time
}

// NewFailover returns a Failover. Pass time.Now outside of tests.
func NewFailover(primary, fallback Limiter, retryInterval time.Duration, now func() time.Time, log *slog.Logger) *Failover {
	return &Failover{primary: primary, fallback: fallback, retryInterval: retryInterval, now: now, log: log}
}

// Allow implements Limiter.
func (f *Failover) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	if f.shouldTryPrimary() {
		result, err := f.primary.Allow(ctx, key, limit, window)
		if err == nil {
			f.markUp()

			return result, nil
		}

		// A client that disconnects mid-request cancels the context; that is not an outage.
		if ctx.Err() == nil {
			f.markDown(err)
		}
	}

	return f.fallback.Allow(ctx, key, limit, window)
}

// shouldTryPrimary reports whether this request should try the primary. While it is down, only
// one request per retryInterval does, and the rest go straight to the fallback.
func (f *Failover) shouldTryPrimary() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.down {
		return true
	}

	now := f.now()
	if now.Before(f.nextProbe) {
		return false
	}

	f.nextProbe = now.Add(f.retryInterval)

	return true
}

func (f *Failover) markDown(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.down {
		f.log.Warn("redis rate limiter unavailable, using the in-memory limiter", "error", err)
	}

	f.down = true
	f.nextProbe = f.now().Add(f.retryInterval)
}

func (f *Failover) markUp() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.down {
		f.log.Info("redis rate limiter recovered")
	}

	f.down = false
}
