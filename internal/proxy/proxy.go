// Package proxy implements the gateway's HTTP handler: it authenticates the
// caller's API key against the rate limiter, consults the response cache,
// then forwards the request to the first healthy upstream, retrying with
// backoff and falling back down the provider list on failure.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stepn0/llm-gateway/internal/breaker"
	"github.com/Stepn0/llm-gateway/internal/cache"
	"github.com/Stepn0/llm-gateway/internal/metrics"
	"github.com/Stepn0/llm-gateway/internal/ratelimit"
)

// Upstream is one OpenAI-compatible provider. Upstreams are tried in the
// order given; the first one whose breaker is closed serves the request.
type Upstream struct {
	Name    string
	BaseURL string           // includes the version prefix, e.g. https://api.openai.com/v1
	APIKey  string           // sent as "Authorization: Bearer"; empty omits the header
	Timeout time.Duration    // per attempt, until response headers arrive; 0 means none
	Breaker *breaker.Breaker // optional
}

// Cached is a buffered, non-streaming upstream response.
type Cached struct {
	Status int
	Header http.Header
	Body   []byte
}

// Options configures a Handler. Only Upstreams is required.
type Options struct {
	Upstreams []Upstream
	Limiter   *ratelimit.Limiter   // nil disables rate limiting
	Cache     *cache.Cache[Cached] // nil disables caching
	Registry  *metrics.Registry    // nil creates a private registry
	Logger    *slog.Logger         // nil discards logs

	MaxAttempts  int           // attempts per upstream before falling back; default 3
	BaseBackoff  time.Duration // delay before the first retry; default 100ms
	MaxBackoff   time.Duration // cap on the backoff; default 2s
	MaxBodyBytes int64         // request body limit; default 4 MiB

	Transport http.RoundTripper                          // default http.DefaultTransport
	Sleep     func(context.Context, time.Duration) error // injectable for tests
}

// Handler is the gateway's http.Handler.
type Handler struct {
	o      Options
	client *http.Client

	requests    *metrics.CounterVec // upstream, status
	retries     *metrics.Counter
	cacheResult *metrics.CounterVec // result
	rateLimited *metrics.Counter
	breakerSkip *metrics.CounterVec // upstream
	latency     *metrics.Histogram
}

// New builds a Handler and registers its metrics.
func New(o Options) *Handler {
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 3
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 100 * time.Millisecond
	}
	if o.MaxBackoff < o.BaseBackoff {
		o.MaxBackoff = max(o.BaseBackoff, 2*time.Second)
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 4 << 20
	}
	if o.Transport == nil {
		o.Transport = http.DefaultTransport
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Registry == nil {
		o.Registry = metrics.NewRegistry()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := o.Registry
	return &Handler{
		o: o,
		client: &http.Client{
			Transport: o.Transport,
			// Hand redirects back to the client instead of following them.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		requests:    r.CounterVec("llm_gateway_requests_total", "Requests completed, by upstream that served them and HTTP status.", "upstream", "status"),
		retries:     r.Counter("llm_gateway_retries_total", "Retried upstream attempts."),
		cacheResult: r.CounterVec("llm_gateway_cache_requests_total", "Cache lookups for cacheable requests.", "result"),
		rateLimited: r.Counter("llm_gateway_rate_limited_total", "Requests rejected with 429 by the rate limiter."),
		breakerSkip: r.CounterVec("llm_gateway_breaker_skipped_total", "Upstreams skipped because their circuit breaker was open.", "upstream"),
		latency:     r.Histogram("llm_gateway_request_duration_seconds", "End-to-end request latency.", []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}),
	}
}

// outcome is what one request did, for metrics and the access log.
type outcome struct {
	upstream string
	status   int
	retries  int
	cache    string // hit, miss or bypass
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	out := &outcome{upstream: "none", cache: "bypass"}
	defer func() {
		d := time.Since(start)
		h.latency.Observe(d.Seconds())
		h.requests.With(out.upstream, strconv.Itoa(out.status)).Inc()
		h.o.Logger.Info("request",
			"method", r.Method, "path", r.URL.Path, "upstream", out.upstream, "status", out.status,
			"latency", d.Round(10*time.Microsecond), "cache", out.cache, "retries", out.retries)
	}()

	if h.o.Limiter != nil {
		if ok, retryAfter := h.o.Limiter.Allow(clientKey(r)); !ok {
			h.rateLimited.Inc()
			out.status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.999)))
			writeError(w, out.status, "rate limit exceeded", "rate_limit_exceeded")
			return
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.o.MaxBodyBytes))
	if err != nil {
		out.status = http.StatusBadRequest
		if errors.As(err, new(*http.MaxBytesError)) {
			out.status = http.StatusRequestEntityTooLarge
		}
		writeError(w, out.status, err.Error(), "invalid_request_error")
		return
	}

	// Only the two fields the gateway itself cares about; the upstream
	// validates the rest.
	var meta struct {
		Stream      bool     `json:"stream"`
		Temperature *float64 `json:"temperature"`
	}
	_ = json.Unmarshal(body, &meta)

	cacheKey := ""
	deterministic := meta.Temperature != nil && *meta.Temperature == 0
	if h.o.Cache != nil && !meta.Stream && (deterministic || r.Header.Get("X-Cache") == "true") {
		if k, err := cache.Key(body); err == nil {
			cacheKey = k
			if c, ok := h.o.Cache.Get(k); ok {
				out.cache, out.status, out.upstream = "hit", c.Status, "cache"
				h.cacheResult.With("hit").Inc()
				copyHeader(w.Header(), c.Header)
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(c.Status)
				w.Write(c.Body)
				return
			}
			out.cache = "miss"
			h.cacheResult.With("miss").Inc()
		}
	}

	var failures []string
	for _, up := range h.o.Upstreams {
		if up.Breaker != nil && !up.Breaker.Allow() {
			h.breakerSkip.With(up.Name).Inc()
			failures = append(failures, up.Name+": circuit open")
			continue
		}
		resp, err := h.tryUpstream(r, up, body, meta.Stream, out)
		if err != nil {
			if r.Context().Err() != nil {
				out.status = 499 // client went away; nothing left to write
				return
			}
			failures = append(failures, up.Name+": "+err.Error())
			continue
		}
		out.upstream = up.Name
		h.writeResponse(w, resp, meta.Stream, cacheKey, out)
		return
	}

	out.status = http.StatusBadGateway
	writeError(w, out.status, "all upstreams failed: "+strings.Join(failures, "; "), "upstream_error")
}

// tryUpstream sends the request to one upstream, retrying retryable failures
// with exponential backoff until MaxAttempts is reached or the breaker trips.
func (h *Handler) tryUpstream(r *http.Request, up Upstream, body []byte, stream bool, out *outcome) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= h.o.MaxAttempts; attempt++ {
		if attempt > 1 {
			if up.Breaker != nil && !up.Breaker.Allow() {
				return nil, fmt.Errorf("%w after %d attempts", errCircuitOpen, attempt-1)
			}
			out.retries++
			h.retries.Inc()
			if err := h.o.Sleep(r.Context(), h.backoff(attempt-1)); err != nil {
				return nil, err
			}
		}

		resp, err := h.send(r, up, body, stream)
		if err != nil {
			lastErr = err
		} else if retryable(resp.StatusCode) {
			lastErr = fmt.Errorf("upstream returned %d", resp.StatusCode)
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // let the connection be reused
			resp.Body.Close()
		} else {
			if up.Breaker != nil {
				up.Breaker.Success()
			}
			return resp, nil
		}
		if up.Breaker != nil {
			up.Breaker.Failure()
		}
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
	}
	return nil, lastErr
}

var errCircuitOpen = errors.New("circuit opened")

// send performs a single attempt. The upstream timeout covers the time until
// response headers arrive; after that, streaming bodies are allowed to run as
// long as the client stays connected.
func (h *Handler) send(r *http.Request, up Upstream, body []byte, stream bool) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	var timer *time.Timer
	if up.Timeout > 0 {
		timer = time.AfterFunc(up.Timeout, cancel)
	}

	target := strings.TrimSuffix(up.BaseURL, "/") + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	copyHeader(req.Header, r.Header)
	for _, k := range []string{"Authorization", "Accept-Encoding", "Content-Length", "X-Cache", "Host"} {
		req.Header.Del(k)
	}
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil && r.Context().Err() == nil {
			return nil, fmt.Errorf("timeout after %s", up.Timeout)
		}
		return nil, unwrapURLError(err)
	}
	if stream && timer != nil {
		timer.Stop()
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// writeResponse relays an upstream response to the client, caching it when
// the request was cacheable and the upstream answered 200.
func (h *Handler) writeResponse(w http.ResponseWriter, resp *http.Response, stream bool, cacheKey string, out *outcome) {
	defer resp.Body.Close()
	out.status = resp.StatusCode
	copyHeader(w.Header(), resp.Header)
	if cacheKey != "" {
		w.Header().Set("X-Cache", "MISS")
	}

	if cacheKey != "" && resp.StatusCode == http.StatusOK {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			out.status = http.StatusBadGateway
			writeError(w, out.status, "reading upstream response: "+err.Error(), "upstream_error")
			return
		}
		h.o.Cache.Set(cacheKey, Cached{Status: resp.StatusCode, Header: w.Header().Clone(), Body: b})
		w.WriteHeader(resp.StatusCode)
		w.Write(b)
		return
	}

	w.WriteHeader(resp.StatusCode)
	var dst io.Writer = w
	if f, ok := w.(http.Flusher); ok && (stream || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")) {
		dst = flushWriter{w, f}
	}
	io.Copy(dst, resp.Body)
}

// backoff returns the delay before retry n (1-based): exponential growth
// capped at MaxBackoff, with "equal jitter" so a herd of retries spreads out.
func (h *Handler) backoff(n int) time.Duration {
	d := h.o.MaxBackoff
	if shift := n - 1; shift < 30 && h.o.BaseBackoff<<shift < h.o.MaxBackoff {
		d = h.o.BaseBackoff << shift
	}
	half := d / 2
	return half + rand.N(half+1)
}

// retryable reports whether a status code is worth retrying: server errors
// and throttling. Other 4xx are the client's fault and are passed through.
func retryable(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests
}

// clientKey identifies the caller for rate limiting: the bearer token if
// present, otherwise the client IP.
func clientKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return "key:" + strings.TrimSpace(auth[len("Bearer "):])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}

// hopByHop headers are meaningful only for a single connection and must not
// be forwarded (RFC 7230 section 6.1).
var hopByHop = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	for _, k := range hopByHop {
		dst.Del(k)
	}
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": typ}})
}

func unwrapURLError(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok || u.Unwrap() == nil {
			return err
		}
		err = u.Unwrap()
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// flushWriter flushes after every write so SSE tokens reach the client as
// soon as the upstream emits them.
type flushWriter struct {
	io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.Writer.Write(p)
	fw.f.Flush()
	return n, err
}

// cancelOnClose releases the per-attempt context when the body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}
