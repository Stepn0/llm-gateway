// Package config loads and validates the gateway's JSON configuration.
//
// Durations are plain integers with the unit in the field name (timeoutMs,
// ttlSeconds) so the file stays readable without a custom duration type.
// API keys are never stored in the file: each provider names an environment
// variable and the key is read from there at startup.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"
)

// Config is the top-level gateway configuration.
type Config struct {
	Listen    string     `json:"listen"`
	RateLimit RateLimit  `json:"rateLimit"`
	Cache     Cache      `json:"cache"`
	Retry     Retry      `json:"retry"`
	Breaker   Breaker    `json:"breaker"`
	Providers []Provider `json:"providers"`
}

// RateLimit configures the per-API-key token bucket. RPS of 0 disables it.
type RateLimit struct {
	RPS              float64 `json:"rps"`
	Burst            int     `json:"burst"`
	IdleEvictSeconds int     `json:"idleEvictSeconds"`
}

// IdleEvict is how long a key may be idle before its bucket is dropped.
func (r RateLimit) IdleEvict() time.Duration { return time.Duration(r.IdleEvictSeconds) * time.Second }

// Cache configures the response cache. Size of 0 disables it.
type Cache struct {
	Size       int `json:"size"`
	TTLSeconds int `json:"ttlSeconds"`
}

// TTL is the lifetime of a cached response.
func (c Cache) TTL() time.Duration { return time.Duration(c.TTLSeconds) * time.Second }

// Retry configures per-upstream retries.
type Retry struct {
	MaxAttempts   int `json:"maxAttempts"`
	BaseBackoffMs int `json:"baseBackoffMs"`
	MaxBackoffMs  int `json:"maxBackoffMs"`
}

// BaseBackoff is the delay before the first retry.
func (r Retry) BaseBackoff() time.Duration { return time.Duration(r.BaseBackoffMs) * time.Millisecond }

// MaxBackoff caps the exponential backoff.
func (r Retry) MaxBackoff() time.Duration { return time.Duration(r.MaxBackoffMs) * time.Millisecond }

// Breaker configures the per-upstream circuit breaker.
type Breaker struct {
	FailureThreshold int `json:"failureThreshold"`
	OpenSeconds      int `json:"openSeconds"`
}

// OpenFor is how long a tripped breaker rejects requests before probing.
func (b Breaker) OpenFor() time.Duration { return time.Duration(b.OpenSeconds) * time.Second }

// Provider is one OpenAI-compatible upstream, listed in priority order.
type Provider struct {
	Name      string `json:"name"`
	BaseURL   string `json:"baseURL"`
	APIKeyEnv string `json:"apiKeyEnv"`
	TimeoutMs int    `json:"timeoutMs"`

	// APIKey is resolved from APIKeyEnv at load time and never serialized.
	APIKey string `json:"-"`
}

// Timeout is the per-attempt time allowed for the upstream to start responding.
func (p Provider) Timeout() time.Duration { return time.Duration(p.TimeoutMs) * time.Millisecond }

// Default returns the configuration used for every field the file omits.
func Default() Config {
	return Config{
		Listen:    ":8080",
		RateLimit: RateLimit{RPS: 10, Burst: 20, IdleEvictSeconds: 600},
		Cache:     Cache{Size: 1024, TTLSeconds: 300},
		Retry:     Retry{MaxAttempts: 3, BaseBackoffMs: 100, MaxBackoffMs: 2000},
		Breaker:   Breaker{FailureThreshold: 5, OpenSeconds: 30},
	}
}

// Load reads and validates the JSON file at path, resolving API keys from
// the process environment.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, os.Getenv)
}

// Parse decodes JSON from r on top of Default, resolves API keys with getenv
// and validates the result. Unknown fields are an error so typos surface at
// startup rather than silently falling back to defaults.
func Parse(r io.Reader, getenv func(string) string) (*Config, error) {
	cfg := Default()
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.TimeoutMs == 0 {
			p.TimeoutMs = 30000
		}
		if p.APIKeyEnv != "" {
			p.APIKey = getenv(p.APIKeyEnv)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return errors.New("listen must not be empty")
	}
	if c.RateLimit.RPS < 0 {
		return errors.New("rateLimit.rps must not be negative")
	}
	if c.RateLimit.RPS > 0 && c.RateLimit.Burst < 1 {
		return errors.New("rateLimit.burst must be at least 1 when rps is set")
	}
	if c.Cache.Size < 0 {
		return errors.New("cache.size must not be negative")
	}
	if c.Cache.Size > 0 && c.Cache.TTLSeconds < 1 {
		return errors.New("cache.ttlSeconds must be at least 1 when the cache is enabled")
	}
	if c.Retry.MaxAttempts < 1 {
		return errors.New("retry.maxAttempts must be at least 1")
	}
	if c.Retry.BaseBackoffMs < 0 || c.Retry.MaxBackoffMs < c.Retry.BaseBackoffMs {
		return errors.New("retry.maxBackoffMs must be >= retry.baseBackoffMs >= 0")
	}
	if c.Breaker.FailureThreshold < 1 {
		return errors.New("breaker.failureThreshold must be at least 1")
	}
	if c.Breaker.OpenSeconds < 1 {
		return errors.New("breaker.openSeconds must be at least 1")
	}
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d]: name is required", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
		if u, err := url.Parse(p.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("provider %s: baseURL must be an absolute http(s) URL, got %q", p.Name, p.BaseURL)
		}
		if p.TimeoutMs < 0 {
			return fmt.Errorf("provider %s: timeoutMs must be positive", p.Name)
		}
		if p.APIKeyEnv != "" && p.APIKey == "" {
			return fmt.Errorf("provider %s: environment variable %s is not set", p.Name, p.APIKeyEnv)
		}
	}
	return nil
}
