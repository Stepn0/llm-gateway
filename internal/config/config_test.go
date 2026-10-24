package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultsAppliedToMinimalConfig(t *testing.T) {
	cfg, err := Parse(strings.NewReader(`{"providers":[{"name":"local","baseURL":"http://localhost:11434/v1"}]}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.RateLimit.RPS != 10 || cfg.RateLimit.Burst != 20 || cfg.RateLimit.IdleEvict() != 10*time.Minute {
		t.Errorf("RateLimit = %+v", cfg.RateLimit)
	}
	if cfg.Cache.Size != 1024 || cfg.Cache.TTL() != 5*time.Minute {
		t.Errorf("Cache = %+v", cfg.Cache)
	}
	if cfg.Retry.MaxAttempts != 3 || cfg.Retry.BaseBackoff() != 100*time.Millisecond || cfg.Retry.MaxBackoff() != 2*time.Second {
		t.Errorf("Retry = %+v", cfg.Retry)
	}
	if cfg.Breaker.FailureThreshold != 5 || cfg.Breaker.OpenFor() != 30*time.Second {
		t.Errorf("Breaker = %+v", cfg.Breaker)
	}
	p := cfg.Providers[0]
	if p.Timeout() != 30*time.Second || p.APIKey != "" {
		t.Errorf("Provider = %+v", p)
	}
}

func TestFullConfigAndAPIKeyFromEnv(t *testing.T) {
	src := `{
	  "listen": "127.0.0.1:9000",
	  "rateLimit": {"rps": 2.5, "burst": 5, "idleEvictSeconds": 60},
	  "cache": {"size": 10, "ttlSeconds": 1},
	  "retry": {"maxAttempts": 4, "baseBackoffMs": 50, "maxBackoffMs": 500},
	  "breaker": {"failureThreshold": 2, "openSeconds": 5},
	  "providers": [
	    {"name": "openai", "baseURL": "https://api.openai.com/v1", "apiKeyEnv": "OPENAI_API_KEY", "timeoutMs": 1500},
	    {"name": "ollama", "baseURL": "http://localhost:11434/v1"}
	  ]
	}`
	cfg, err := Parse(strings.NewReader(src), env(map[string]string{"OPENAI_API_KEY": "sk-test"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" || cfg.RateLimit.RPS != 2.5 || cfg.Cache.TTL() != time.Second ||
		cfg.Retry.MaxAttempts != 4 || cfg.Breaker.OpenFor() != 5*time.Second {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if got := cfg.Providers[0]; got.APIKey != "sk-test" || got.Timeout() != 1500*time.Millisecond {
		t.Errorf("openai provider = %+v", got)
	}
	if cfg.Providers[1].APIKey != "" {
		t.Errorf("ollama should have no key, got %q", cfg.Providers[1].APIKey)
	}
}

func TestExplicitZeroDisablesOptionalFeatures(t *testing.T) {
	cfg, err := Parse(strings.NewReader(`{"rateLimit":{"rps":0},"cache":{"size":0},"providers":[{"name":"a","baseURL":"http://a/v1"}]}`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit.RPS != 0 || cfg.Cache.Size != 0 {
		t.Fatalf("explicit zeros must be preserved, got %+v %+v", cfg.RateLimit, cfg.Cache)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"no providers":      {`{}`, "at least one provider"},
		"unnamed provider":  {`{"providers":[{"baseURL":"http://a/v1"}]}`, "name is required"},
		"duplicate name":    {`{"providers":[{"name":"a","baseURL":"http://a/v1"},{"name":"a","baseURL":"http://b/v1"}]}`, `duplicate provider name "a"`},
		"bad url":           {`{"providers":[{"name":"a","baseURL":"localhost:11434"}]}`, "baseURL must be an absolute http(s) URL"},
		"missing env":       {`{"providers":[{"name":"a","baseURL":"http://a/v1","apiKeyEnv":"NOPE"}]}`, `environment variable NOPE is not set`},
		"negative timeout":  {`{"providers":[{"name":"a","baseURL":"http://a/v1","timeoutMs":-1}]}`, "timeoutMs must be positive"},
		"zero attempts":     {`{"retry":{"maxAttempts":0},"providers":[{"name":"a","baseURL":"http://a/v1"}]}`, "retry.maxAttempts must be at least 1"},
		"backoff order":     {`{"retry":{"baseBackoffMs":500,"maxBackoffMs":100},"providers":[{"name":"a","baseURL":"http://a/v1"}]}`, "retry.maxBackoffMs must be >= retry.baseBackoffMs"},
		"rps without burst": {`{"rateLimit":{"rps":1,"burst":0},"providers":[{"name":"a","baseURL":"http://a/v1"}]}`, "rateLimit.burst must be at least 1"},
		"negative cache":    {`{"cache":{"size":-1},"providers":[{"name":"a","baseURL":"http://a/v1"}]}`, "cache.size must not be negative"},
		"breaker threshold": {`{"breaker":{"failureThreshold":0},"providers":[{"name":"a","baseURL":"http://a/v1"}]}`, "breaker.failureThreshold must be at least 1"},
		"unknown field":     {`{"listne":":1","providers":[{"name":"a","baseURL":"http://a/v1"}]}`, `unknown field "listne"`},
		"invalid json":      {`{`, "unexpected EOF"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.src), env(nil))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/config.json"); err == nil {
		t.Fatal("expected error for missing file")
	}
}
