package app

import (
	"testing"
	"time"
)

func TestTokenBucketBoundsBurstsAndRefills(t *testing.T) {
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	limiter := newTokenBucket(2, time.Minute, func() time.Time { return now })
	if !limiter.allow() || !limiter.allow() || limiter.allow() {
		t.Fatal("burst limit was not enforced")
	}
	now = now.Add(30 * time.Second)
	if !limiter.allow() || limiter.allow() {
		t.Fatal("token refill was not enforced")
	}
}
