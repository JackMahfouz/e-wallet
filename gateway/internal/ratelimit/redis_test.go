package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newRedisLimiter starts an in-process Redis, so expiry is driven by FastForward rather than by
// real waiting.
func newRedisLimiter(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	client := NewRedisClient(ClientConfig{Addr: mr.Addr(), Timeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })

	return NewRedis(client, "test:", 100*time.Millisecond), mr
}

func TestRedisAllowsUpToTheLimitThenBlocksUntilTheWindowEnds(t *testing.T) {
	limiter, mr := newRedisLimiter(t)

	for i := 1; i <= 2; i++ {
		if result := mustAllow(t, limiter, "k", 2, 10*time.Second); !result.Allowed {
			t.Fatalf("request %d blocked, want allowed", i)
		}
	}

	result := mustAllow(t, limiter, "k", 2, 10*time.Second)
	if result.Allowed || result.RetryAfter != 10*time.Second {
		t.Fatalf("3rd request = %+v, want blocked with RetryAfter 10s", result)
	}

	mr.FastForward(4 * time.Second)
	result = mustAllow(t, limiter, "k", 2, 10*time.Second)
	if result.Allowed || result.RetryAfter != 6*time.Second {
		t.Errorf("4s later = %+v, want blocked with RetryAfter 6s", result)
	}

	mr.FastForward(6 * time.Second)
	if !mustAllow(t, limiter, "k", 2, 10*time.Second).Allowed {
		t.Error("request after the window ended was blocked, want allowed")
	}
}

func TestRedisWindowStartsAtTheFirstRequestAndIsNotExtendedByTraffic(t *testing.T) {
	limiter, mr := newRedisLimiter(t)
	mustAllow(t, limiter, "k", 2, 10*time.Second)

	mr.FastForward(4 * time.Second)
	mustAllow(t, limiter, "k", 2, 10*time.Second)

	// If every request refreshed the expiry this would report 10s.
	result := mustAllow(t, limiter, "k", 2, 10*time.Second)
	if result.Allowed || result.RetryAfter != 6*time.Second {
		t.Errorf("result = %+v, want blocked with RetryAfter 6s", result)
	}
}

func TestRedisKeysArePrefixedAndIndependent(t *testing.T) {
	limiter, mr := newRedisLimiter(t)

	mustAllow(t, limiter, "a", 1, time.Minute)
	blocked := !mustAllow(t, limiter, "a", 1, time.Minute).Allowed
	otherAllowed := mustAllow(t, limiter, "b", 1, time.Minute).Allowed

	if !blocked || !otherAllowed {
		t.Errorf("a blocked = %v, b allowed = %v; want true, true", blocked, otherAllowed)
	}
	if !mr.Exists("test:a") || !mr.Exists("test:b") {
		t.Errorf("keys = %v, want test:a and test:b", mr.Keys())
	}
}

func TestRedisEveryCounterHasAnExpiry(t *testing.T) {
	limiter, mr := newRedisLimiter(t)

	mustAllow(t, limiter, "k", 5, time.Minute)

	if got := mr.TTL("test:k"); got != time.Minute {
		t.Errorf("TTL = %v, want 1m", got)
	}
}

func TestRedisRepairsACounterThatLostItsExpiry(t *testing.T) {
	limiter, mr := newRedisLimiter(t)
	if err := mr.Set("test:k", "9"); err != nil {
		t.Fatalf("seed counter: %v", err)
	}

	result := mustAllow(t, limiter, "k", 3, time.Minute)

	if result.Allowed {
		t.Error("request allowed, want blocked: the counter already exceeds the limit")
	}
	if got := mr.TTL("test:k"); got <= 0 {
		t.Errorf("TTL = %v, want an expiry to have been set", got)
	}
}

func TestRedisFailsFastWhenUnreachable(t *testing.T) {
	limiter, mr := newRedisLimiter(t)
	mr.Close()

	start := time.Now()
	_, err := limiter.Allow(context.Background(), "k", 5, time.Minute)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Allow succeeded against a closed Redis")
	}
	// The configured timeout is 100ms; client-side dial retries would blow well past it.
	if elapsed > 400*time.Millisecond {
		t.Errorf("a failed Redis call took %v, want it to fail within about the 100ms timeout", elapsed)
	}
}

func TestRedisReturnsAnErrorWhenUnreachable(t *testing.T) {
	limiter, mr := newRedisLimiter(t)
	mr.Close()

	result, err := limiter.Allow(context.Background(), "k", 5, time.Minute)

	if err == nil {
		t.Fatalf("Allow = %+v, want an error", result)
	}
	if result.Allowed {
		t.Error("an errored result must not claim the request is allowed")
	}
}
