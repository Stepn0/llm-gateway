package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stepn0/llm-gateway/internal/breaker"
	"github.com/Stepn0/llm-gateway/internal/cache"
	"github.com/Stepn0/llm-gateway/internal/ratelimit"
)

const body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

// fakeUpstream is an httptest server that counts requests.
type fakeUpstream struct {
	*httptest.Server
	calls atomic.Int32
}

func newFakeUpstream(t *testing.T, h http.HandlerFunc) *fakeUpstream {
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func completion(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hello from %s"}}]}`, name)
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", code) }
}

func newHandler(t *testing.T, o Options, ups ...*fakeUpstream) *Handler {
	t.Helper()
	for i, u := range ups {
		o.Upstreams = append(o.Upstreams, Upstream{
			Name:    fmt.Sprintf("up%d", i+1),
			BaseURL: u.URL + "/v1",
			APIKey:  fmt.Sprintf("key%d", i+1),
			Timeout: 2 * time.Second,
		})
	}
	if o.Sleep == nil {
		o.Sleep = func(context.Context, time.Duration) error { return nil }
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 1
	}
	return New(o)
}

func post(h http.Handler, payload string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-key")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestForwardsRequestAndResponse(t *testing.T) {
	var seen struct {
		path, auth, ctype, accept string
		body                      string
	}
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.path, seen.auth, seen.ctype, seen.body = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)
		seen.accept = r.Header.Get("Accept-Encoding")
		w.Header().Set("X-Request-Id", "abc")
		w.WriteHeader(http.StatusCreated)
		completion("primary")(w, r)
	})
	rec := post(newHandler(t, Options{}, up), body, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if seen.path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", seen.path)
	}
	if seen.auth != "Bearer key1" {
		t.Errorf("upstream Authorization = %q, want the configured upstream key", seen.auth)
	}
	if seen.ctype != "application/json" || seen.body != body {
		t.Errorf("request not forwarded intact: ctype=%q body=%q", seen.ctype, seen.body)
	}
	if seen.accept != "gzip" {
		t.Errorf("client Accept-Encoding should not be forwarded; transport should negotiate gzip itself, got %q", seen.accept)
	}
	if rec.Header().Get("X-Request-Id") != "abc" || !strings.Contains(rec.Body.String(), "hello from primary") {
		t.Errorf("response not forwarded intact: %v %s", rec.Header(), rec.Body)
	}
	if rec.Header().Get("X-Cache") != "" {
		t.Errorf("non-deterministic request must not carry X-Cache, got %q", rec.Header().Get("X-Cache"))
	}
}

func TestRetriesOn503ThenSucceeds(t *testing.T) {
	var slept []time.Duration
	var n atomic.Int32
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			status(http.StatusServiceUnavailable)(w, r)
			return
		}
		completion("primary")(w, r)
	})
	h := newHandler(t, Options{
		MaxAttempts: 3, BaseBackoff: 100 * time.Millisecond, MaxBackoff: time.Second,
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	}, up)
	rec := post(h, body, nil)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello from primary") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if up.calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3", up.calls.Load())
	}
	if len(slept) != 2 {
		t.Fatalf("slept %d times, want 2", len(slept))
	}
	// Equal jitter: attempt n sleeps within [base*2^(n-1)/2, base*2^(n-1)].
	if slept[0] < 50*time.Millisecond || slept[0] > 100*time.Millisecond {
		t.Errorf("first backoff = %v, want within [50ms,100ms]", slept[0])
	}
	if slept[1] < 100*time.Millisecond || slept[1] > 200*time.Millisecond {
		t.Errorf("second backoff = %v, want within [100ms,200ms]", slept[1])
	}
}

func TestFallsBackToSecondProvider(t *testing.T) {
	down := newFakeUpstream(t, status(http.StatusInternalServerError))
	ok := newFakeUpstream(t, completion("fallback"))
	rec := post(newHandler(t, Options{MaxAttempts: 2}, down, ok), body, nil)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello from fallback") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if down.calls.Load() != 2 || ok.calls.Load() != 1 {
		t.Fatalf("calls: down=%d ok=%d, want 2 and 1", down.calls.Load(), ok.calls.Load())
	}
}

func TestFallsBackOnNetworkError(t *testing.T) {
	dead := httptest.NewServer(nil)
	dead.Close() // connection refused
	ok := newFakeUpstream(t, completion("fallback"))
	h := New(Options{
		Upstreams: []Upstream{{Name: "dead", BaseURL: dead.URL + "/v1"}, {Name: "ok", BaseURL: ok.URL + "/v1"}},
		Sleep:     func(context.Context, time.Duration) error { return nil },
	})
	rec := post(h, body, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello from fallback") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestReturns502WhenAllFail(t *testing.T) {
	a := newFakeUpstream(t, status(http.StatusBadGateway))
	b := newFakeUpstream(t, status(http.StatusTooManyRequests))
	rec := post(newHandler(t, Options{}, a, b), body, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var e struct {
		Error struct{ Message, Type string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body is not JSON: %s", rec.Body)
	}
	if e.Error.Type != "upstream_error" || !strings.Contains(e.Error.Message, "up1: upstream returned 502") || !strings.Contains(e.Error.Message, "up2: upstream returned 429") {
		t.Fatalf("error = %+v", e.Error)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	up := newFakeUpstream(t, status(http.StatusBadRequest))
	rec := post(newHandler(t, Options{MaxAttempts: 3}, up), body, nil)
	if rec.Code != http.StatusBadRequest || up.calls.Load() != 1 {
		t.Fatalf("status = %d calls = %d; a 400 must pass through untouched", rec.Code, up.calls.Load())
	}
}

func TestRateLimitReturns429(t *testing.T) {
	up := newFakeUpstream(t, completion("primary"))
	h := newHandler(t, Options{Limiter: ratelimit.New(1, 1)}, up)

	if rec := post(h, body, nil); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d", rec.Code)
	}
	rec := post(h, body, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("body = %s", rec.Body)
	}
	// A different key has its own bucket.
	if rec := post(h, body, map[string]string{"Authorization": "Bearer other"}); rec.Code != http.StatusOK {
		t.Fatalf("other key: status = %d", rec.Code)
	}
	if up.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (limited request must not reach upstream)", up.calls.Load())
	}
}

func TestCacheHitOnDeterministicRequest(t *testing.T) {
	up := newFakeUpstream(t, completion("primary"))
	h := newHandler(t, Options{Cache: cache.New[Cached](16, time.Minute)}, up)
	det := `{"model":"gpt-4o","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	first := post(h, det, nil)
	if first.Code != http.StatusOK || first.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first: status=%d X-Cache=%q", first.Code, first.Header().Get("X-Cache"))
	}
	// Same request, different key order and whitespace.
	second := post(h, `{ "temperature": 0, "messages": [{"content":"hi","role":"user"}], "model": "gpt-4o" }`, nil)
	if second.Code != http.StatusOK || second.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second: status=%d X-Cache=%q", second.Code, second.Header().Get("X-Cache"))
	}
	if second.Body.String() != first.Body.String() || second.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("cached response differs: %s", second.Body)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}

	// Non-zero temperature bypasses the cache entirely.
	if rec := post(h, body, nil); rec.Header().Get("X-Cache") != "" || up.calls.Load() != 2 {
		t.Fatalf("temperature>0 should bypass cache: X-Cache=%q calls=%d", rec.Header().Get("X-Cache"), up.calls.Load())
	}
	// X-Cache: true opts in regardless of temperature.
	post(h, body, map[string]string{"X-Cache": "true"})
	if rec := post(h, body, map[string]string{"X-Cache": "true"}); rec.Header().Get("X-Cache") != "HIT" || up.calls.Load() != 3 {
		t.Fatalf("X-Cache: true should cache: X-Cache=%q calls=%d", rec.Header().Get("X-Cache"), up.calls.Load())
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	up := newFakeUpstream(t, status(http.StatusBadRequest))
	h := newHandler(t, Options{Cache: cache.New[Cached](16, time.Minute)}, up)
	det := `{"temperature":0}`
	post(h, det, nil)
	post(h, det, nil)
	if up.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (4xx must not be cached)", up.calls.Load())
	}
}

func TestBreakerOpensAndUpstreamIsSkipped(t *testing.T) {
	down := newFakeUpstream(t, status(http.StatusInternalServerError))
	ok := newFakeUpstream(t, completion("fallback"))
	h := newHandler(t, Options{}, down, ok)
	h.o.Upstreams[0].Breaker = breaker.New(2, time.Hour)

	for i := 0; i < 2; i++ {
		if rec := post(h, body, nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i+1, rec.Code)
		}
	}
	if down.calls.Load() != 2 {
		t.Fatalf("down calls = %d, want 2", down.calls.Load())
	}
	if st := h.o.Upstreams[0].Breaker.State(); st != breaker.Open {
		t.Fatalf("breaker state = %v, want open", st)
	}
	rec := post(h, body, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello from fallback") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if down.calls.Load() != 2 {
		t.Fatalf("open breaker must skip the upstream, but it was called %d times", down.calls.Load())
	}
}

func TestBreakerStopsRetryLoopEarly(t *testing.T) {
	down := newFakeUpstream(t, status(http.StatusInternalServerError))
	ok := newFakeUpstream(t, completion("fallback"))
	h := newHandler(t, Options{MaxAttempts: 5}, down, ok)
	h.o.Upstreams[0].Breaker = breaker.New(2, time.Hour)

	if rec := post(h, body, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if down.calls.Load() != 2 {
		t.Fatalf("down calls = %d, want 2: retries must stop once the breaker trips", down.calls.Load())
	}
}

func TestStreamingPassesThroughAndFlushes(t *testing.T) {
	chunks := []string{`data: {"choices":[{"delta":{"content":"Hel"}}]}`, `data: {"choices":[{"delta":{"content":"lo"}}]}`, `data: [DONE]`}
	up := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
			w.(http.Flusher).Flush()
		}
	})
	h := newHandler(t, Options{Cache: cache.New[Cached](16, time.Minute)}, up)
	rec := post(h, `{"model":"gpt-4o","temperature":0,"stream":true,"messages":[]}`, nil)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
	}
	if want := strings.Join(chunks, "\n\n") + "\n\n"; rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
	if !rec.Flushed {
		t.Fatal("streaming response must be flushed")
	}
	if rec.Header().Get("X-Cache") != "" {
		t.Fatal("streaming responses must bypass the cache")
	}
	post(h, `{"model":"gpt-4o","temperature":0,"stream":true,"messages":[]}`, nil)
	if up.calls.Load() != 2 {
		t.Fatalf("streaming response was served from cache")
	}
}

func TestUpstreamTimeoutFallsBack(t *testing.T) {
	slow := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	ok := newFakeUpstream(t, completion("fallback"))
	h := New(Options{
		Upstreams: []Upstream{
			{Name: "slow", BaseURL: slow.URL + "/v1", Timeout: 50 * time.Millisecond},
			{Name: "ok", BaseURL: ok.URL + "/v1"},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	start := time.Now()
	rec := post(h, body, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fallback") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout not enforced, took %v", time.Since(start))
	}
}

func TestRejectsOversizedBody(t *testing.T) {
	up := newFakeUpstream(t, completion("primary"))
	h := newHandler(t, Options{MaxBodyBytes: 16}, up)
	rec := post(h, body, nil)
	if rec.Code != http.StatusRequestEntityTooLarge || up.calls.Load() != 0 {
		t.Fatalf("status = %d calls = %d", rec.Code, up.calls.Load())
	}
}

func TestMetricsAreRecorded(t *testing.T) {
	up := newFakeUpstream(t, completion("primary"))
	h := newHandler(t, Options{Limiter: ratelimit.New(1, 1), Cache: cache.New[Cached](16, time.Minute)}, up)
	post(h, `{"temperature":0}`, nil)
	post(h, `{"temperature":0}`, nil)

	var sb strings.Builder
	h.o.Registry.Render(&sb)
	out := sb.String()
	for _, want := range []string{
		`llm_gateway_requests_total{upstream="up1",status="200"} 1`,
		`llm_gateway_requests_total{upstream="none",status="429"} 1`,
		`llm_gateway_rate_limited_total 1`,
		`llm_gateway_cache_requests_total{result="miss"} 1`,
		`llm_gateway_request_duration_seconds_count 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q\n%s", want, out)
		}
	}
}
