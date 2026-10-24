// Package ratelimit implements a per-key token-bucket rate limiter.
//
// Each key (typically an API key) owns a bucket that holds up to burst
// tokens and refills continuously at rate tokens per second. A request
// consumes one token; when the bucket is empty the request is denied and
// the caller is told how long to wait.
package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock replaces the time source. Tests use it to advance time without sleeping.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) { l.now = now }
}

// Limiter is a set of token buckets indexed by key. It is safe for concurrent use.
type Limiter struct {
	rate  float64 // tokens added per second
	burst float64 // maximum tokens a bucket can hold
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time // last time tokens was brought up to date
}

// New returns a Limiter that admits up to burst requests at once and rps
// requests per second sustained, per key.
func New(rps float64, burst int, opts ...Option) *Limiter {
	l := &Limiter{
		rate:    rps,
		burst:   float64(burst),
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Allow consumes one token from key's bucket. If no token is available it
// returns false and the duration until the next token will be.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, exists := l.buckets[key]
	if !exists {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	// Lazy refill: tokens accrue only when the bucket is touched, so idle
	// keys cost nothing until the janitor evicts them.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	deficit := 1 - b.tokens
	return false, time.Duration(deficit / l.rate * float64(time.Second))
}

// Evict removes buckets that have not been touched for at least idle and
// returns how many were removed. A bucket idle that long has fully refilled,
// so dropping it is indistinguishable from keeping it.
func (l *Limiter) Evict(idle time.Duration) int {
	cutoff := l.now().Add(-idle)

	l.mu.Lock()
	defer l.mu.Unlock()

	n := 0
	for k, b := range l.buckets {
		if !b.last.After(cutoff) {
			delete(l.buckets, k)
			n++
		}
	}
	return n
}

// Len returns the number of tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// RunJanitor evicts idle buckets every interval until ctx is cancelled.
// It blocks; run it in its own goroutine.
func (l *Limiter) RunJanitor(ctx context.Context, interval, idle time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Evict(idle)
		}
	}
}
