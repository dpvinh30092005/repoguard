// Package metrics is a tiny Prometheus-text-format counter registry, enough to
// alert on webhook failures and queue saturation without a client library.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type Registry struct {
	mu       sync.Mutex
	counters map[string]*atomic.Int64
	gauges   map[string]func() int64
}

func New() *Registry {
	return &Registry{counters: map[string]*atomic.Int64{}, gauges: map[string]func() int64{}}
}

// Inc increments counter name with labels given as alternating key, value.
func (r *Registry) Inc(name string, labels ...string) {
	key := name + fmtLabels(labels)
	r.mu.Lock()
	c, ok := r.counters[key]
	if !ok {
		c = &atomic.Int64{}
		r.counters[key] = c
	}
	r.mu.Unlock()
	c.Add(1)
}

// Gauge registers a function sampled at scrape time.
func (r *Registry) Gauge(name string, fn func() int64) {
	r.mu.Lock()
	r.gauges[name] = fn
	r.mu.Unlock()
}

// Get returns a counter's value (for tests).
func (r *Registry) Get(name string, labels ...string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name+fmtLabels(labels)]; ok {
		return c.Load()
	}
	return 0
}

func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	lines := make([]string, 0, len(r.counters)+len(r.gauges))
	for k, c := range r.counters {
		lines = append(lines, fmt.Sprintf("%s %d", k, c.Load()))
	}
	for k, fn := range r.gauges {
		lines = append(lines, fmt.Sprintf("%s %d", k, fn()))
	}
	r.mu.Unlock()
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

func fmtLabels(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", kv[i], kv[i+1]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
