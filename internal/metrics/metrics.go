// Package metrics is a minimal, dependency-free metrics registry that
// renders the Prometheus text exposition format.
//
// It supports exactly what a gateway needs: counters, label-partitioned
// counters and a fixed-bucket histogram. All updates are lock-free atomics
// except label lookup, which takes a short mutex.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing integer.
type Counter struct{ n atomic.Int64 }

// Inc adds one.
func (c *Counter) Inc() { c.n.Add(1) }

// Add adds n.
func (c *Counter) Add(n int64) { c.n.Add(n) }

// Value returns the current count.
func (c *Counter) Value() int64 { return c.n.Load() }

// CounterVec is a family of counters partitioned by label values.
type CounterVec struct {
	labels []string
	mu     sync.Mutex
	series map[string]*Counter // key: label values joined with \x00
}

// With returns the counter for the given label values (one per label name).
func (v *CounterVec) With(values ...string) *Counter {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: got %d label values for %d labels", len(values), len(v.labels)))
	}
	key := strings.Join(values, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.series[key]
	if !ok {
		c = &Counter{}
		v.series[key] = c
	}
	return c
}

// Histogram counts observations into cumulative fixed buckets.
type Histogram struct {
	bounds []float64 // upper bounds, ascending; +Inf is implicit
	counts []atomic.Int64
	count  atomic.Int64
	sum    atomic.Uint64 // float64 bits
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i].Add(1)
		}
	}
	h.count.Add(1)
	for {
		old := h.sum.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sum.CompareAndSwap(old, next) {
			return
		}
	}
}

type metric struct {
	name, help string
	write      func(w io.Writer)
}

// Registry owns a set of metrics and renders them in registration order.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Counter registers and returns a new counter.
func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{}
	r.add(name, help, "counter", func(w io.Writer) {
		fmt.Fprintf(w, "%s %d\n", name, c.Value())
	})
	return c
}

// CounterVec registers and returns a new labelled counter family.
func (r *Registry) CounterVec(name, help string, labels ...string) *CounterVec {
	v := &CounterVec{labels: labels, series: make(map[string]*Counter)}
	r.add(name, help, "counter", func(w io.Writer) {
		v.mu.Lock()
		keys := make([]string, 0, len(v.series))
		for k := range v.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		type row struct {
			key string
			n   int64
		}
		rows := make([]row, 0, len(keys))
		for _, k := range keys {
			rows = append(rows, row{k, v.series[k].Value()})
		}
		v.mu.Unlock()

		for _, rw := range rows {
			values := strings.Split(rw.key, "\x00")
			parts := make([]string, len(values))
			for i, val := range values {
				parts[i] = labels[i] + `="` + escape(val) + `"`
			}
			fmt.Fprintf(w, "%s{%s} %d\n", name, strings.Join(parts, ","), rw.n)
		}
	})
	return v
}

// Histogram registers and returns a histogram with the given ascending upper
// bounds. A +Inf bucket is added automatically.
func (r *Registry) Histogram(name, help string, bounds []float64) *Histogram {
	h := &Histogram{bounds: bounds, counts: make([]atomic.Int64, len(bounds))}
	r.add(name, help, "histogram", func(w io.Writer) {
		for i, b := range h.bounds {
			fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", name, formatFloat(b), h.counts[i].Load())
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, h.count.Load())
		fmt.Fprintf(w, "%s_sum %s\n", name, formatFloat(math.Float64frombits(h.sum.Load())))
		fmt.Fprintf(w, "%s_count %d\n", name, h.count.Load())
	})
	return h
}

func (r *Registry) add(name, help, typ string, body func(io.Writer)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, metric{name: name, help: help, write: func(w io.Writer) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		body(w)
	}})
}

// Render writes all metrics in Prometheus text format.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// Handler serves the registry over HTTP.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Render(w)
	})
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
