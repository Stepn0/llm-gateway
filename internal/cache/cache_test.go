package cache

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestCache(capacity int, ttl time.Duration) (*Cache[string], *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return New[string](capacity, ttl, WithClock(clock.Now)), clock
}

func TestHitAndMiss(t *testing.T) {
	c, _ := newTestCache(2, time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Set("a", "1")
	v, ok := c.Get("a")
	if !ok || v != "1" {
		t.Fatalf("got (%q,%v), want (\"1\",true)", v, ok)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c, _ := newTestCache(2, time.Minute)
	c.Set("a", "1")
	c.Set("b", "2")
	c.Get("a") // a is now most recently used
	c.Set("c", "3")

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted as least recently used")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%s should still be cached", k)
		}
	}
	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.Len())
	}
}

func TestSetExistingKeyRefreshesRecency(t *testing.T) {
	c, _ := newTestCache(2, time.Minute)
	c.Set("a", "1")
	c.Set("b", "2")
	c.Set("a", "1b") // overwrite, a becomes most recent
	c.Set("c", "3")  // evicts b
	if v, ok := c.Get("a"); !ok || v != "1b" {
		t.Fatalf("a = (%q,%v), want updated value", v, ok)
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
}

func TestTTLExpiry(t *testing.T) {
	c, clock := newTestCache(10, time.Minute)
	c.Set("a", "1")
	clock.Advance(59 * time.Second)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("entry should be live before TTL")
	}
	clock.Advance(2 * time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry should have expired")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry should be removed, len = %d", c.Len())
	}
}

func TestKeyIsCanonical(t *testing.T) {
	a, err := Key([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Key([]byte("{ \"temperature\": 0,\n \"messages\": [ {\"content\":\"hi\", \"role\":\"user\"} ],\n \"model\": \"gpt-4o\" }"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("key order and whitespace must not affect the key:\n%s\n%s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected hex sha256, got %q", a)
	}

	c, _ := Key([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi!"}],"temperature":0}`))
	if a == c {
		t.Fatal("different content must produce different keys")
	}

	// Large integers must survive canonicalization without float rounding.
	d, _ := Key([]byte(`{"seed":9007199254740993}`))
	e, _ := Key([]byte(`{"seed":9007199254740992}`))
	if d == e {
		t.Fatal("canonicalization lost integer precision")
	}
}

func TestKeyRejectsInvalidJSON(t *testing.T) {
	for _, body := range []string{``, `{`, `{"a":1} trailing`} {
		if _, err := Key([]byte(body)); err == nil {
			t.Fatalf("Key(%q) should fail", body)
		}
	}
}
