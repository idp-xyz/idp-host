package observability

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics (M5): counters, histograms and gauge callbacks exposed in the Prometheus text format on a
// separate listener (IDP_METRICS_LISTEN), never on the data plane port. Written by hand: the
// exposition format is a few dozen lines and keeps the client library out of the supply chain.
// Label values never carry subjects, tokens, arguments or URIs (targets, methods and outcomes only).

// Metrics is a registry of instruments.
type Metrics struct {
	mu         sync.Mutex
	counters   map[string]*CounterVec
	histograms map[string]*HistogramVec
	gauges     map[string]*gaugeFunc
	start      time.Time
}

// NewMetrics returns an empty registry with process_start_time_seconds and go_goroutines.
func NewMetrics() *Metrics {
	m := &Metrics{counters: map[string]*CounterVec{}, histograms: map[string]*HistogramVec{}, gauges: map[string]*gaugeFunc{}, start: time.Now()}
	m.GaugeFunc("process_start_time_seconds", "Start time of the process since unix epoch in seconds.", nil,
		func() []LabeledValue { return []LabeledValue{{Value: float64(m.start.Unix())}} })
	m.GaugeFunc("go_goroutines", "Number of goroutines that currently exist.", nil,
		func() []LabeledValue { return []LabeledValue{{Value: float64(runtime.NumGoroutine())}} })
	return m
}

// CounterVec is a monotonically increasing counter with labels.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64 // key: joined label values
}

// Inc adds 1 for the label values.
func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds v for the label values.
func (c *CounterVec) Add(v float64, labelValues ...string) {
	if len(labelValues) != len(c.labels) {
		return
	}
	c.mu.Lock()
	c.values[key(labelValues)] += v
	c.mu.Unlock()
}

// HistogramVec is a cumulative histogram with labels.
type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64 // upper bounds, ascending
	mu         sync.Mutex
	series     map[string]*histogram
}

type histogram struct {
	counts []float64 // per bucket (not cumulative)
	sum    float64
	count  float64
}

// Observe records v for the label values.
func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	if len(labelValues) != len(h.labels) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := key(labelValues)
	s := h.series[k]
	if s == nil {
		s = &histogram{counts: make([]float64, len(h.buckets))}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

// LabeledValue is one sample of a gauge.
type LabeledValue struct {
	Labels []string
	Value  float64
}

type gaugeFunc struct {
	help   string
	labels []string
	fn     func() []LabeledValue
}

// DurationBuckets suit request latencies from 1 ms to ~1 min.
var DurationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Counter registers (or returns) a counter.
func (m *Metrics) Counter(name, help string, labels ...string) *CounterVec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.counters[name]; ok {
		return c
	}
	c := &CounterVec{name: name, help: help, labels: labels, values: map[string]float64{}}
	m.counters[name] = c
	return c
}

// Histogram registers (or returns) a histogram.
func (m *Metrics) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.histograms[name]; ok {
		return h
	}
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*histogram{}}
	m.histograms[name] = h
	return h
}

// GaugeFunc registers a gauge whose samples are computed at scrape time.
func (m *Metrics) GaugeFunc(name, help string, labels []string, fn func() []LabeledValue) {
	m.mu.Lock()
	m.gauges[name] = &gaugeFunc{help: help, labels: labels, fn: fn}
	m.mu.Unlock()
}

const sep = "\x00"

func key(values []string) string { return strings.Join(values, sep) }

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func labelSet(names, values []string, extra ...string) string {
	if len(names)+len(extra) == 0 {
		return ""
	}
	var parts []string
	for i, n := range names {
		parts = append(parts, n+`="`+escape(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+escape(extra[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func fmtFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Write renders the exposition.
func (m *Metrics) Write(w *strings.Builder) {
	m.mu.Lock()
	counters, hists, gauges := m.counters, m.histograms, m.gauges
	m.mu.Unlock()
	names := make([]string, 0, len(counters)+len(hists)+len(gauges))
	for n := range counters {
		names = append(names, n)
	}
	for n := range hists {
		names = append(names, n)
	}
	for n := range gauges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if c, ok := counters[n]; ok {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", n, escape(c.help), n)
			c.mu.Lock()
			keys := make([]string, 0, len(c.values))
			for k := range c.values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "%s%s %s\n", n, labelSet(c.labels, strings.Split(k, sep)), fmtFloat(c.values[k]))
			}
			c.mu.Unlock()
		}
		if h, ok := hists[n]; ok {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", n, escape(h.help), n)
			h.mu.Lock()
			keys := make([]string, 0, len(h.series))
			for k := range h.series {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				s := h.series[k]
				values := strings.Split(k, sep)
				if k == "" {
					values = nil
				}
				cum := 0.0
				for i, b := range h.buckets {
					cum += s.counts[i]
					fmt.Fprintf(w, "%s_bucket%s %s\n", n, labelSet(h.labels, values, "le", fmtFloat(b)), fmtFloat(cum))
				}
				fmt.Fprintf(w, "%s_bucket%s %s\n", n, labelSet(h.labels, values, "le", "+Inf"), fmtFloat(s.count))
				fmt.Fprintf(w, "%s_sum%s %s\n", n, labelSet(h.labels, values), fmtFloat(s.sum))
				fmt.Fprintf(w, "%s_count%s %s\n", n, labelSet(h.labels, values), fmtFloat(s.count))
			}
			h.mu.Unlock()
		}
		if g, ok := gauges[n]; ok {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", n, escape(g.help), n)
			for _, lv := range g.fn() {
				fmt.Fprintf(w, "%s%s %s\n", n, labelSet(g.labels, lv.Labels), fmtFloat(lv.Value))
			}
		}
	}
}

// Handler serves the exposition.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		m.Write(&b)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

// HTTPMetrics counts requests by route pattern and status and observes their latency.
func HTTPMetrics(m *Metrics, prefix string, next http.Handler) http.Handler {
	total := m.Counter(prefix+"_http_requests_total", "HTTP requests by route and status.", "route", "status")
	dur := m.Histogram(prefix+"_http_request_duration_seconds", "HTTP request latency by route.", DurationBuckets, "route")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		total.Inc(route, strconv.Itoa(sw.status))
		dur.Observe(time.Since(start).Seconds(), route)
	})
}

// ServeMetrics starts a listener for /metrics (and /healthz) on addr; "" disables it.
func ServeMetrics(addr string, m *Metrics, log interface{ Info(string, ...any) }) *http.Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "metrics listener: %v\n", err)
		}
	}()
	log.Info("metrics listening", "addr", addr)
	return srv
}
