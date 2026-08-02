package app

import (
	"sync"
	"time"
)

type tokenBucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	refill   float64
	last     time.Time
	now      func() time.Time
}

func newTokenBucket(capacity int, refillPeriod time.Duration, now func() time.Time) *tokenBucket {
	if now == nil {
		now = time.Now
	}
	return &tokenBucket{
		capacity: float64(capacity),
		tokens:   float64(capacity),
		refill:   float64(capacity) / refillPeriod.Seconds(),
		last:     now(),
		now:      now,
	}
}

func (limiter *tokenBucket) allow() bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.now()
	limiter.tokens = min(limiter.capacity, limiter.tokens+now.Sub(limiter.last).Seconds()*limiter.refill)
	limiter.last = now
	if limiter.tokens < 1 {
		return false
	}
	limiter.tokens--
	return true
}
