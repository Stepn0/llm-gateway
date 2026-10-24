package ratelimit

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(rps float64, burst int) (*Limiter, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return New(rps, burst, WithClock(clock.Now)), clock
}

func TestAllowsBurstThenDenies(t *testing.T) {
	l, _ := newTestLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d should be allowed within burst", i+1)
		}
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("4th request should be denied")
	}
	if retry != time.Second {
		t.Fatalf("retryAfter = %v, want 1s", retry)
	}
}

func TestRefillsOverTime(t *testing.T) {
	l, clock := newTestLimiter(2, 1) // 2 tokens/s, so one token every 500ms
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first request should be allowed")
	}
	if ok, retry := l.Allow("k"); ok || retry != 500*time.Millisecond {
		t.Fatalf("got ok=%v retry=%v, want denied with 500ms", ok, retry)
	}
	clock.Advance(250 * time.Millisecond)
	if ok, retry := l.Allow("k"); ok || retry != 250*time.Millisecond {
		t.Fatalf("after 250ms: got ok=%v retry=%v, want denied with 250ms", ok, retry)
	}
	clock.Advance(250 * time.Millisecond)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("after a full refill interval the request should be allowed")
	}
}

func TestRefillIsCappedAtBurst(t *testing.T) {
	l, clock := newTestLimiter(10, 2)
	l.Allow("k")
	clock.Advance(time.Hour)
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("bucket must not accumulate more than burst tokens")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	l.Allow("a")
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a should be exhausted")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b should have its own bucket")
	}
}

func TestEvictIdleBuckets(t *testing.T) {
	l, clock := newTestLimiter(1, 1)
	l.Allow("old")
	clock.Advance(time.Minute)
	l.Allow("fresh")
	if n := l.Evict(30 * time.Second); n != 1 {
		t.Fatalf("evicted %d buckets, want 1", n)
	}
	if l.Len() != 1 {
		t.Fatalf("len = %d, want 1", l.Len())
	}
	// The evicted key starts over with a full bucket.
	if ok, _ := l.Allow("old"); !ok {
		t.Fatal("evicted key should get a fresh bucket")
	}
}
