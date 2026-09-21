package timing

import (
	"sync"
	"testing"
	"time"
)

func TestSamples_PercentilesOverTheWindow(t *testing.T) {
	s := New(100)
	// 1ms..100ms, recorded out of order so sorting is actually doing work.
	for i := 100; i >= 1; i-- {
		s.Record(time.Duration(i) * time.Millisecond)
	}

	got := s.Snapshot()
	if got.Count != 100 {
		t.Errorf("count = %d, want 100", got.Count)
	}
	if got.P50 < 45*time.Millisecond || got.P50 > 55*time.Millisecond {
		t.Errorf("p50 = %s, want around 50ms", got.P50)
	}
	if got.P90 < 85*time.Millisecond || got.P90 > 95*time.Millisecond {
		t.Errorf("p90 = %s, want around 90ms", got.P90)
	}
}

// TestSamples_CountOutlivesTheWindow — the count is what tells an
// operator whether a percentile is worth believing. A p50 drawn from
// four samples and one drawn from four thousand look identical without
// it, and this process rarely lives long enough to gather many.
func TestSamples_CountOutlivesTheWindow(t *testing.T) {
	s := New(8)
	for range 50 {
		s.Record(10 * time.Millisecond)
	}

	got := s.Snapshot()
	if got.Count != 50 {
		t.Errorf("count = %d, want 50 — it counts everything recorded, not what is still retained", got.Count)
	}
	if got.P50 != 10*time.Millisecond {
		t.Errorf("p50 = %s, want 10ms", got.P50)
	}
}

// TestSamples_WindowKeepsTheRecentOnes — an old, slow era must not go
// on dragging the percentile down after things improved, which is the
// whole reason this is a window and not a running average.
func TestSamples_WindowKeepsTheRecentOnes(t *testing.T) {
	s := New(4)
	for range 4 {
		s.Record(900 * time.Millisecond)
	}
	for range 4 {
		s.Record(10 * time.Millisecond)
	}

	if got := s.Snapshot().P50; got != 10*time.Millisecond {
		t.Errorf("p50 = %s after the window turned over, want 10ms — stale samples are being kept", got)
	}
}

func TestSamples_EmptyAndNilAreHarmless(t *testing.T) {
	if got := New(4).Snapshot(); got.Count != 0 || got.P50 != 0 {
		t.Errorf("empty snapshot = %+v, want zeroes", got)
	}
	// A nil *Samples is what a Client built without instrumentation
	// holds; recording into one must not panic on a hot path.
	var nilSamples *Samples
	nilSamples.Record(time.Second)
	if got := nilSamples.Snapshot(); got.Count != 0 {
		t.Errorf("nil snapshot = %+v, want zeroes", got)
	}
}

func TestSamples_ConcurrentRecording(t *testing.T) {
	s := New(64)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s.Record(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	if got := s.Snapshot().Count; got != 1000 {
		t.Errorf("count = %d after 20x50 concurrent records, want 1000", got)
	}
}
