package breaker

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(threshold int, openFor time.Duration) (*Breaker, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return New(threshold, openFor, WithClock(clock.Now)), clock
}

func TestStartsClosedAndAllows(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)
	if b.State() != Closed {
		t.Fatalf("state = %v, want Closed", b.State())
	}
	if !b.Allow() {
		t.Fatal("closed breaker must allow")
	}
}

func TestOpensAfterThresholdFailures(t *testing.T) {
	b, _ := newTestBreaker(3, time.Minute)
	b.Failure()
	b.Failure()
	if b.State() != Closed {
		t.Fatal("should stay closed below threshold")
	}
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %v, want Open after 3 failures", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker must reject")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b, _ := newTestBreaker(2, time.Minute)
	b.Failure()
	b.Success()
	b.Failure()
	if b.State() != Closed {
		t.Fatal("a success in between should have reset the count")
	}
}

func TestHalfOpenAllowsSingleProbe(t *testing.T) {
	b, clock := newTestBreaker(1, time.Minute)
	b.Failure()
	clock.Advance(59 * time.Second)
	if b.Allow() {
		t.Fatal("still within open window")
	}
	clock.Advance(2 * time.Second)
	if b.State() != HalfOpen {
		t.Fatalf("state = %v, want HalfOpen after open window", b.State())
	}
	if !b.Allow() {
		t.Fatal("half-open breaker should allow one probe")
	}
	if b.Allow() {
		t.Fatal("only one probe may be in flight while half-open")
	}
}

func TestProbeSuccessCloses(t *testing.T) {
	b, clock := newTestBreaker(1, time.Minute)
	b.Failure()
	clock.Advance(2 * time.Minute)
	b.Allow()
	b.Success()
	if b.State() != Closed {
		t.Fatalf("state = %v, want Closed after successful probe", b.State())
	}
	if !b.Allow() {
		t.Fatal("closed breaker must allow")
	}
}

func TestProbeFailureReopens(t *testing.T) {
	b, clock := newTestBreaker(1, time.Minute)
	b.Failure()
	clock.Advance(2 * time.Minute)
	b.Allow()
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %v, want Open after failed probe", b.State())
	}
	clock.Advance(30 * time.Second)
	if b.Allow() {
		t.Fatal("open window should have restarted at the failed probe")
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{Closed: "closed", Open: "open", HalfOpen: "half-open"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}
