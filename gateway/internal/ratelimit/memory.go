package ratelimit

import (
	"context"
	"sync"
	"time"
)

const sweepInterval = time.Minute

// Memory is a Limiter that keeps its counters in this process. Counts are per gateway
// instance, so it is the fallback for when Redis is unavailable.
type Memory struct {
	now       func() time.Time
	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
}

type entry struct {
	count   int
	resetAt time.Time
}

// NewMemory returns an in-memory limiter. Pass time.Now outside of tests.
func NewMemory(now func() time.Time) *Memory {
	return &Memory{now: now, entries: make(map[string]*entry), lastSweep: now()}
}

// Allow implements Limiter. It never returns an error.
func (m *Memory) Allow(_ context.Context, key string, limit int, window time.Duration) (Result, error) {
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.sweep(now)

	e, ok := m.entries[key]
	if !ok || !now.Before(e.resetAt) {
		e = &entry{resetAt: now.Add(window)}
		m.entries[key] = e
	}

	e.count++
	if e.count > limit {
		return Result{Allowed: false, RetryAfter: e.resetAt.Sub(now)}, nil
	}

	return Result{Allowed: true}, nil
}

// sweep drops expired windows at most once per sweepInterval, so memory does not grow without
// bound and no background goroutine is needed. The caller holds m.mu.
func (m *Memory) sweep(now time.Time) {
	if now.Sub(m.lastSweep) < sweepInterval {
		return
	}

	for key, e := range m.entries {
		if !now.Before(e.resetAt) {
			delete(m.entries, key)
		}
	}
	m.lastSweep = now
}
