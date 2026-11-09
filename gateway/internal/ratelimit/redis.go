package ratelimit

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ClientConfig is what NewRedisClient needs to connect.
type ClientConfig struct {
	Addr     string
	DB       int
	Password string
	TLS      bool
	// Timeout bounds dialing, reads and writes.
	Timeout time.Duration
}

// NewRedisClient returns a client tuned to fail fast. Retries are disabled and dialing is
// attempted once: Failover already handles an outage, so retrying inside the client would only
// make every request slower while Redis is down.
func NewRedisClient(c ClientConfig) *redis.Client {
	options := &redis.Options{
		Addr:               c.Addr,
		DB:                 c.DB,
		Password:           c.Password,
		DialTimeout:        c.Timeout,
		ReadTimeout:        c.Timeout,
		WriteTimeout:       c.Timeout,
		MaxRetries:         -1,
		DialerRetries:      1,
		DialerRetryTimeout: time.Millisecond,
	}
	if c.TLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return redis.NewClient(options)
}

// Redis is a Limiter whose counters live in Redis, shared by every gateway instance.
type Redis struct {
	client  redis.UniversalClient
	prefix  string
	timeout time.Duration
}

// NewRedis returns a Redis limiter. Every Redis call is bounded by timeout.
func NewRedis(client redis.UniversalClient, prefix string, timeout time.Duration) *Redis {
	return &Redis{client: client, prefix: prefix, timeout: timeout}
}

// Allow implements Limiter.
//
// INCR, PEXPIRE NX and PTTL run in one MULTI, so they are atomic: the window starts at the
// first request (NX sets the expiry only when the key has none), a counter can never be left
// without an expiry, and the remaining TTL is exactly the time until the window ends.
func (r *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	redisKey := r.prefix + key

	var count *redis.IntCmd
	var ttl *redis.DurationCmd

	_, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		count = pipe.Incr(ctx, redisKey)
		pipe.Do(ctx, "PEXPIRE", redisKey, window.Milliseconds(), "NX")
		ttl = pipe.PTTL(ctx, redisKey)

		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("redis rate limit: %w", err)
	}

	if count.Val() <= int64(limit) {
		return Result{Allowed: true}, nil
	}

	retryAfter := ttl.Val()
	if retryAfter <= 0 {
		retryAfter = window
	}

	return Result{Allowed: false, RetryAfter: retryAfter}, nil
}
