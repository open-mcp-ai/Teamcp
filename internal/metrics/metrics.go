// Package metrics is the smallest thing that makes an instance observable.
//
// It is hand-rolled rather than a client library because the surface is four
// counters and one histogram-shaped concern (wait latency). Pulling in a Prometheus
// client to expose five numbers would cost more binary than the whole server.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Registry holds named counters.
type Registry struct {
	mu     sync.Mutex
	values map[string]float64
	start  time.Time
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{values: map[string]float64{}, start: time.Now()}
}

// Inc adds one to a counter. Nil-safe so callers never branch on "metrics on?".
func (r *Registry) Inc(name string) { r.Add(name, 1) }

// Add adds n to a counter.
func (r *Registry) Add(name string, n float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.values[name] += n
	r.mu.Unlock()
}

// Handler exposes the registry in Prometheus text format.
//
// The HELP/TYPE lines are omitted on purpose: a counter whose meaning needs a
// paragraph is better documented next to where it is incremented.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r == nil {
			http.Error(w, "metrics disabled", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		r.mu.Lock()
		names := make([]string, 0, len(r.values))
		for k := range r.values {
			names = append(names, k)
		}
		sort.Strings(names)
		snapshot := make(map[string]float64, len(r.values))
		for k, v := range r.values {
			snapshot[k] = v
		}
		start := r.start
		r.mu.Unlock()

		for _, k := range names {
			fmt.Fprintf(w, "%s %g\n", k, snapshot[k])
		}
		fmt.Fprintf(w, "teamcp_uptime_seconds %g\n", time.Since(start).Seconds())
	})
}
