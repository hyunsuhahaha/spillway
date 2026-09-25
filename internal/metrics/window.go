// Package metrics implements a sliding time window of request samples used to
// compute latency percentiles and throughput.
package metrics

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Sample is a single observed request.
type Sample struct {
	At    time.Time
	Dur   time.Duration
	OK    bool
	Group string
}

// Stats summarises the samples inside the window.
type Stats struct {
	Count     int     `json:"count"`
	RPS       float64 `json:"rps"`
	P50       float64 `json:"p50_ms"`
	P95       float64 `json:"p95_ms"`
	P99       float64 `json:"p99_ms"`
	Errors    int     `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
}

// Window keeps samples for a fixed span of time.
type Window struct {
	mu      sync.Mutex
	span    time.Duration
	samples []Sample
}

// NewWindow creates a window covering span.
func NewWindow(span time.Duration) *Window {
	return &Window{span: span}
}

// Add records a sample.
func (w *Window) Add(s Sample) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples = append(w.samples, s)
	w.prune(s.At)
}

func (w *Window) prune(now time.Time) {
	cut := now.Add(-w.span)
	i := sort.Search(len(w.samples), func(i int) bool { return !w.samples[i].At.Before(cut) })
	if i == 0 {
		return
	}
	if i > len(w.samples)/2 {
		// Copy so the backing array does not grow without bound.
		w.samples = append([]Sample(nil), w.samples[i:]...)
	} else {
		w.samples = w.samples[i:]
	}
}

// Stats returns statistics for the given group ("" means all samples).
func (w *Window) Stats(group string) Stats {
	w.mu.Lock()
	now := time.Now()
	w.prune(now)
	durs := make([]float64, 0, len(w.samples))
	var st Stats
	for _, s := range w.samples {
		if group != "" && s.Group != group {
			continue
		}
		st.Count++
		if !s.OK {
			st.Errors++
		}
		durs = append(durs, float64(s.Dur)/float64(time.Millisecond))
	}
	w.mu.Unlock()

	if st.Count == 0 {
		return st
	}
	sort.Float64s(durs)
	st.P50 = percentile(durs, 0.50)
	st.P95 = percentile(durs, 0.95)
	st.P99 = percentile(durs, 0.99)
	st.RPS = round2(float64(st.Count) / w.span.Seconds())
	st.ErrorRate = round2(float64(st.Errors) / float64(st.Count))
	return st
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return round2(sorted[idx])
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
