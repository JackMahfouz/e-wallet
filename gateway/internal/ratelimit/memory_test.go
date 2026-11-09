package ratelimit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newClock() *clock { return &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)} }

func mustAllow(t *testing.T, l Limiter, key string, limit int, window time.Duration) Result {
	t.Helper()

	result, err := l.Allow(context.Background(), key, limit, window)
	if err != nil {
		t.Fatalf("Allow(%q): %v", key, err)
	}

	return result
}

func TestMemoryAllowsUpToTheLimitThenBlocks(t *testing.T) {
	c := newClock()
	m := NewMemory(c.Now)

	for i := 1; i <= 3; i++ {
		if result := mustAllow(t, m, "k", 3, time.Minute); !result.Allowed {
			t.Fatalf("request %d blocked, want allowed", i)
		}
	}

	result := mustAllow(t, m, "k", 3, time.Minute)
	if result.Allowed || result.RetryAfter != time.Minute {
		t.Errorf("4th request = %+v, want blocked with RetryAfter 1m", result)
	}

	// Blocked requests do not extend the window: the retry time keeps shrinking.
	c.Advance(20 * time.Second)
	result = mustAllow(t, m, "k", 3, time.Minute)
	if result.Allowed || result.RetryAfter != 40*time.Second {
		t.Errorf("after 20s = %+v, want blocked with RetryAfter 40s", result)
	}
}

func TestMemoryStartsANewWindowWhenTheOldOneEnds(t *testing.T) {
	c := newClock()
	m := NewMemory(c.Now)
	mustAllow(t, m, "k", 1, time.Minute)
	if mustAllow(t, m, "k", 1, time.Minute).Allowed {
		t.Fatal("2nd request allowed, want blocked")
	}

	c.Advance(time.Minute)

	if !mustAllow(t, m, "k", 1, time.Minute).Allowed {
		t.Error("request after the window ended was blocked, want allowed")
	}
	if mustAllow(t, m, "k", 1, time.Minute).Allowed {
		t.Error("the new window did not enforce the limit")
	}
}

func TestMemoryKeysAreIndependent(t *testing.T) {
	m := NewMemory(newClock().Now)
	mustAllow(t, m, "a", 1, time.Minute)

	if mustAllow(t, m, "a", 1, time.Minute).Allowed {
		t.Fatal("key a not blocked")
	}
	if !mustAllow(t, m, "b", 1, time.Minute).Allowed {
		t.Error("key b blocked by key a's traffic")
	}
}

func TestMemorySweepsExpiredWindowsButKeepsLiveOnes(t *testing.T) {
	c := newClock()
	m := NewMemory(c.Now)
	for i := range 100 {
		mustAllow(t, m, fmt.Sprintf("short-%d", i), 5, 30*time.Second)
	}
	mustAllow(t, m, "long", 5, time.Hour)

	c.Advance(2 * sweepInterval)
	mustAllow(t, m, "trigger", 5, time.Minute)

	if got := len(m.entries); got != 2 {
		t.Errorf("entries after sweep = %d, want 2 (the live long window and the new key)", got)
	}
	if _, ok := m.entries["long"]; !ok {
		t.Error("the unexpired window was swept")
	}
}
