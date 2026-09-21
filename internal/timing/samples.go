// Package timing records how long real operations actually took, so the
// constants that model them can be checked against reality instead of
// trusted.
//
// It exists because of a specific mistake. internal/bcp/cost.go's
// OwnOverheadCost sat at 1ms for months: measured correctly, from the
// wrong place. The access log shows what the container spends answering
// a request, and it cannot see the hop in front of the container — so
// every estimate the cost model produced was low by ~149ms and nothing
// said so. The fix was noticing, which took holding an outside
// measurement against an inside one.
//
// This is the inside half, kept permanently: the service times the work
// it was going to do anyway, and an operator can compare that against
// what the model assumes. Nothing here ever triggers work of its own —
// the moment a measurement can cause a request to BCP, it becomes
// traffic they did not ask for.
package timing

import (
	"sort"
	"sync"
	"time"
)

// DefaultWindow is how many recent samples a Samples keeps.
//
// A window rather than a running average, because what matters is what
// this deployment is doing lately — and because a bounded slice is a
// bounded amount of memory, which a growing histogram is not. Small
// enough to be free, large enough that a percentile means something.
const DefaultWindow = 256

// Samples is a fixed-size ring of recent durations, safe for concurrent
// use.
type Samples struct {
	mu     sync.Mutex
	window []time.Duration
	next   int
	filled bool
	// total counts every sample ever recorded, not just the ones still
	// in the window. A percentile drawn from four samples and one drawn
	// from four thousand read identically without it.
	total int64
}

// New builds a Samples keeping the last `window` durations.
func New(window int) *Samples {
	if window <= 0 {
		window = DefaultWindow
	}
	return &Samples{window: make([]time.Duration, window)}
}

// Record adds one observation. Cheap enough to sit on a hot path: a
// lock, an index write, and an increment.
func (s *Samples) Record(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.window[s.next] = d
	s.next = (s.next + 1) % len(s.window)
	if s.next == 0 {
		s.filled = true
	}
	s.total++
}

// Snapshot is what Samples knows, at a moment.
type Snapshot struct {
	// Count is every observation since this process started, which is
	// usually not long: the container sleeps after ten idle minutes and
	// takes these with it. Read a percentile next to a small Count with
	// the suspicion it deserves.
	Count int64         `json:"count"`
	P50   time.Duration `json:"-"`
	P90   time.Duration `json:"-"`
}

// Snapshot reports the current percentiles over the retained window.
func (s *Samples) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	n := len(s.window)
	if !s.filled {
		n = s.next
	}
	held := make([]time.Duration, n)
	copy(held, s.window[:n])
	total := s.total
	s.mu.Unlock()

	if n == 0 {
		return Snapshot{Count: total}
	}
	sort.Slice(held, func(i, j int) bool { return held[i] < held[j] })
	return Snapshot{
		Count: total,
		P50:   held[percentileIndex(n, 50)],
		P90:   held[percentileIndex(n, 90)],
	}
}

// percentileIndex is the nearest-rank index into a sorted slice of n.
func percentileIndex(n, pct int) int {
	if n <= 1 {
		return 0
	}
	idx := (pct*n + 99) / 100 // ceil(pct/100 * n)
	if idx >= n {
		idx = n - 1
	} else if idx > 0 {
		idx--
	}
	return idx
}
