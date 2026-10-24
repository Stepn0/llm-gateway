// Package breaker implements a circuit breaker.
//
// The breaker is closed (requests flow) until it sees threshold consecutive
// failures, then opens (requests are rejected) for openFor. After that it
// becomes half-open: a single probe request is let through, and its outcome
// decides whether the breaker closes again or reopens for another window.
package breaker

import (
	"sync"
	"time"
)

// State is the breaker's position.
type State int

const (
	Closed   State = iota // healthy, requests flow
	Open                  // tripped, requests are rejected
	HalfOpen              // open window elapsed, one probe allowed
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	}
	return "unknown"
}

// Option configures a Breaker.
type Option func(*Breaker)

// WithClock replaces the time source.
func WithClock(now func() time.Time) Option {
	return func(b *Breaker) { b.now = now }
}

// Breaker is a single circuit breaker. It is safe for concurrent use.
type Breaker struct {
	threshold int
	openFor   time.Duration
	now       func() time.Time

	mu       sync.Mutex
	state    State
	failures int       // consecutive failures while closed
	openedAt time.Time // when the breaker last tripped
	probing  bool      // a half-open probe is in flight
}

// New returns a closed breaker that trips after threshold consecutive
// failures and stays open for openFor.
func New(threshold int, openFor time.Duration, opts ...Option) *Breaker {
	b := &Breaker{threshold: threshold, openFor: openFor, now: time.Now}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Allow reports whether a request may be attempted right now. A true result
// from a half-open breaker reserves the single probe slot; the caller must
// report the outcome with Success or Failure.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.current() {
	case Closed:
		return true
	case HalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return false
	}
}

// Success records a successful request and closes the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = Closed
	b.failures = 0
	b.probing = false
}

// Failure records a failed request. Enough of them in a row trip the breaker;
// a failed half-open probe reopens it immediately.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.current() {
	case HalfOpen:
		b.trip()
	case Closed:
		b.failures++
		if b.failures >= b.threshold {
			b.trip()
		}
	}
}

// State returns the breaker's current position, accounting for elapsed time.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.current()
}

// current applies the time-based Open -> HalfOpen transition. Callers hold b.mu.
func (b *Breaker) current() State {
	if b.state == Open && !b.now().Before(b.openedAt.Add(b.openFor)) {
		b.state = HalfOpen
		b.probing = false
	}
	return b.state
}

func (b *Breaker) trip() {
	b.state = Open
	b.openedAt = b.now()
	b.failures = 0
	b.probing = false
}
