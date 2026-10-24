// Package cache provides a small LRU cache with per-entry TTL and a helper
// for deriving stable cache keys from JSON request bodies.
package cache

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

type options struct {
	now func() time.Time
}

// Option configures a Cache.
type Option func(*options)

// WithClock replaces the time source used for TTL checks.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// Cache is a fixed-capacity LRU cache. Entries expire after the TTL given to
// New. It is safe for concurrent use.
type Cache[V any] struct {
	capacity int
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	order *list.List               // front = most recently used
	items map[string]*list.Element // key -> element holding *entry[V]
}

type entry[V any] struct {
	key     string
	value   V
	expires time.Time
}

// New returns a cache holding at most capacity entries, each valid for ttl.
func New[V any](capacity int, ttl time.Duration, opts ...Option) *Cache[V] {
	o := options{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	return &Cache[V]{
		capacity: capacity,
		ttl:      ttl,
		now:      o.now,
		order:    list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// Get returns the live value for key and marks it most recently used.
// Expired entries are removed on access.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[V])
	if !c.now().Before(e.expires) {
		c.remove(el)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

// Set stores value under key, evicting the least recently used entry when
// the cache is full.
func (c *Cache[V]) Set(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	expires := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[V])
		e.value, e.expires = value, expires
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&entry[V]{key: key, value: value, expires: expires})
	for c.order.Len() > c.capacity {
		c.remove(c.order.Back())
	}
}

// Len returns the number of entries, including any not yet noticed as expired.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *Cache[V]) remove(el *list.Element) {
	delete(c.items, el.Value.(*entry[V]).key)
	c.order.Remove(el)
}

// Key returns the hex SHA-256 of body in canonical JSON form: object keys
// sorted, insignificant whitespace removed, numbers kept verbatim. Two
// requests that differ only in formatting therefore share a key.
func Key(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep 9007199254740993 from becoming 9.007199254740992e15
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errors.New("cache: trailing data after JSON value")
	}
	canonical, err := json.Marshal(v) // encoding/json sorts map keys
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
