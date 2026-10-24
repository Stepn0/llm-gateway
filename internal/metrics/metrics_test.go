package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRenderCounters(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("gw_rate_limited_total", "Requests rejected by the rate limiter.")
	v := r.CounterVec("gw_requests_total", "Requests by upstream and status.", "upstream", "status")

	c.Inc()
	c.Add(2)
	v.With("openai", "200").Inc()
	v.With("openai", "200").Inc()
	v.With("ollama", "503").Inc()
	v.With("we\"ird\n", "200").Inc()

	want := `# HELP gw_rate_limited_total Requests rejected by the rate limiter.
# TYPE gw_rate_limited_total counter
gw_rate_limited_total 3
# HELP gw_requests_total Requests by upstream and status.
# TYPE gw_requests_total counter
gw_requests_total{upstream="ollama",status="503"} 1
gw_requests_total{upstream="openai",status="200"} 2
gw_requests_total{upstream="we\"ird\n",status="200"} 1
`
	if got := render(r); got != want {
		t.Fatalf("render mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderHistogram(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("gw_latency_seconds", "Latency.", []float64{0.1, 0.5, 1})
	for _, v := range []float64{0.05, 0.3, 0.3, 2} {
		h.Observe(v)
	}
	want := `# HELP gw_latency_seconds Latency.
# TYPE gw_latency_seconds histogram
gw_latency_seconds_bucket{le="0.1"} 1
gw_latency_seconds_bucket{le="0.5"} 3
gw_latency_seconds_bucket{le="1"} 3
gw_latency_seconds_bucket{le="+Inf"} 4
gw_latency_seconds_sum 2.65
gw_latency_seconds_count 4
`
	if got := render(r); got != want {
		t.Fatalf("render mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestHandlerContentType(t *testing.T) {
	r := NewRegistry()
	r.Counter("x_total", "x").Inc()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "x_total 1\n") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestConcurrentIncrements(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "")
	v := r.CounterVec("v_total", "", "l")
	h := r.Histogram("h", "", []float64{1})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Inc()
				v.With("a").Inc()
				h.Observe(0.5)
			}
		}()
	}
	wg.Wait()
	if c.Value() != 5000 || v.With("a").Value() != 5000 || h.count.Load() != 5000 {
		t.Fatalf("lost updates: %d %d %d", c.Value(), v.With("a").Value(), h.count.Load())
	}
}

func render(r *Registry) string {
	var sb strings.Builder
	r.Render(&sb)
	return sb.String()
}
