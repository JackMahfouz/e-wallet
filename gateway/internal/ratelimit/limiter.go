// Package ratelimit counts requests per key in fixed windows.
//
// A window starts at a key's first request and lasts `window`. Up to `limit` requests are
// allowed in it; the rest are refused until it ends. Fixed windows are simple and cheap, at
// the cost of allowing up to twice the limit across a window boundary.
package ratelimit

import (
	"context"
	"time"
)

// Result is the outcome of counting one request.
type Result struct {
	Allowed bool
	// RetryAfter is how long until the current window ends. Set when Allowed is false.
	RetryAfter time.Duration
}

// Limiter counts a request for key and says whether it is within limit per window.
type Limiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error)
}
