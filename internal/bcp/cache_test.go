package bcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCache_Get exercises every behavior Cache[T] is relied on for
// elsewhere in this package: caching a successful fetch, de-duplicating
// concurrent callers for the same key, never caching a failed fetch, and
// keeping different keys' fetches independent of each other. Each
// scenario is a self-contained subtest (a "row") run via t.Run, which is
// the table-driven shape for a case that needs its own setup/assertions
// rather than a single pure input/output pair.
func TestCache_Get(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "fetches once and returns the value",
			run: func(t *testing.T) {
				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "value-for-" + key, nil
				})

				got, err := c.Get(context.Background(), "k1")
				if err != nil {
					t.Fatalf("Get returned error: %v", err)
				}
				if got != "value-for-k1" {
					t.Errorf("got %q, want %q", got, "value-for-k1")
				}
				if calls != 1 {
					t.Errorf("fetch called %d times, want 1", calls)
				}
			},
		},
		{
			name: "a second call within the refetch window reuses the cached value",
			run: func(t *testing.T) {
				var calls int32
				c := NewCache(func(ctx context.Context, key string) (int, error) {
					atomic.AddInt32(&calls, 1)
					return 42, nil
				})

				for i := 0; i < 3; i++ {
					got, err := c.Get(context.Background(), "same-key")
					if err != nil {
						t.Fatalf("Get #%d returned error: %v", i, err)
					}
					if got != 42 {
						t.Errorf("Get #%d = %d, want 42", i, got)
					}
				}
				if calls != 1 {
					t.Errorf("fetch called %d times across 3 Gets, want 1 (should be cached after the first)", calls)
				}
			},
		},
		{
			name: "concurrent callers for the same key share one fetch",
			run: func(t *testing.T) {
				var calls int32
				started := make(chan struct{})
				release := make(chan struct{})

				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					close(started)
					<-release // hold the fetch open until every caller has joined it
					return "shared", nil
				})

				const n = 5
				results := make([]string, n)
				errs := make([]error, n)
				var wg sync.WaitGroup
				wg.Add(n)
				for i := 0; i < n; i++ {
					go func(i int) {
						defer wg.Done()
						results[i], errs[i] = c.Get(context.Background(), "concurrent-key")
					}(i)
				}

				<-started
				close(release)
				wg.Wait()

				if calls != 1 {
					t.Errorf("fetch called %d times for %d concurrent callers, want 1", calls, n)
				}
				for i, err := range errs {
					if err != nil {
						t.Errorf("caller %d got error: %v", i, err)
					}
					if results[i] != "shared" {
						t.Errorf("caller %d got %q, want %q", i, results[i], "shared")
					}
				}
			},
		},
		{
			name: "a failed fetch is not cached, so the next call retries",
			run: func(t *testing.T) {
				var calls int32
				boom := errors.New("upstream boom")
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					n := atomic.AddInt32(&calls, 1)
					if n == 1 {
						return "", boom
					}
					return "recovered", nil
				})

				_, err := c.Get(context.Background(), "flaky")
				if !errors.Is(err, boom) {
					t.Fatalf("first Get error = %v, want %v", err, boom)
				}

				got, err := c.Get(context.Background(), "flaky")
				if err != nil {
					t.Fatalf("second Get returned error: %v", err)
				}
				if got != "recovered" {
					t.Errorf("second Get = %q, want %q", got, "recovered")
				}
				if calls != 2 {
					t.Errorf("fetch called %d times, want 2 (error must not have been cached)", calls)
				}
			},
		},
		{
			name: "different keys are fetched and cached independently",
			run: func(t *testing.T) {
				calls := make(map[string]int)
				var mu sync.Mutex
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					mu.Lock()
					calls[key]++
					mu.Unlock()
					return "value-" + key, nil
				})

				for _, key := range []string{"a", "b", "a", "b", "c"} {
					got, err := c.Get(context.Background(), key)
					if err != nil {
						t.Fatalf("Get(%q) returned error: %v", key, err)
					}
					want := "value-" + key
					if got != want {
						t.Errorf("Get(%q) = %q, want %q", key, got, want)
					}
				}

				for _, key := range []string{"a", "b", "c"} {
					if calls[key] != 1 {
						t.Errorf("key %q fetched %d times, want 1", key, calls[key])
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

func TestCache_Invalidate(t *testing.T) {
	t.Run("clears the entry once the manual-invalidate floor has elapsed, so the next Get refetches", func(t *testing.T) {
		orig := minManualInvalidateInterval
		minManualInvalidateInterval = 0 // no real sleep needed — see that var's doc comment
		defer func() { minManualInvalidateInterval = orig }()

		var calls int32
		c := NewCache(func(ctx context.Context, key string) (int, error) {
			return int(atomic.AddInt32(&calls, 1)), nil
		})

		first, err := c.Get(context.Background(), "k1")
		if err != nil {
			t.Fatalf("first Get: %v", err)
		}
		if first != 1 {
			t.Fatalf("first Get = %d, want 1", first)
		}

		c.Invalidate("k1")

		second, err := c.Get(context.Background(), "k1")
		if err != nil {
			t.Fatalf("second Get: %v", err)
		}
		if second != 2 {
			t.Errorf("second Get = %d, want 2 (Invalidate should have forced a real refetch)", second)
		}
		if calls != 2 {
			t.Errorf("fetch called %d times, want 2", calls)
		}
	})

	t.Run("does nothing if the entry was fetched more recently than the manual-invalidate floor", func(t *testing.T) {
		var calls int32
		c := NewCache(func(ctx context.Context, key string) (int, error) {
			return int(atomic.AddInt32(&calls, 1)), nil
		})

		if _, err := c.Get(context.Background(), "k1"); err != nil {
			t.Fatalf("first Get: %v", err)
		}

		// Called immediately after — well within the default 2s floor —
		// so this should be a no-op rather than clearing the entry.
		c.Invalidate("k1")

		got, err := c.Get(context.Background(), "k1")
		if err != nil {
			t.Fatalf("second Get: %v", err)
		}
		if got != 1 {
			t.Errorf("second Get = %d, want 1 (still cached — Invalidate should have been throttled)", got)
		}
		if calls != 1 {
			t.Errorf("fetch called %d times, want 1", calls)
		}
	})

	t.Run("invalidating a key that was never fetched is a safe no-op", func(t *testing.T) {
		c := NewCache(func(ctx context.Context, key string) (int, error) { return 1, nil })
		c.Invalidate("never-fetched") // must not panic
	})
}

func TestCache_FetchedAt(t *testing.T) {
	c := NewCache(func(ctx context.Context, key string) (string, error) { return "v", nil })

	if _, ok := c.FetchedAt("k1"); ok {
		t.Error("FetchedAt on a never-fetched key reported ok=true, want false")
	}

	before := time.Now()
	if _, err := c.Get(context.Background(), "k1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	after := time.Now()

	fetchedAt, ok := c.FetchedAt("k1")
	if !ok {
		t.Fatal("FetchedAt reported ok=false right after a successful Get")
	}
	if fetchedAt.Before(before) || fetchedAt.After(after) {
		t.Errorf("FetchedAt = %v, want between %v and %v", fetchedAt, before, after)
	}
}

// TestCache_Get_minRefetchIntervalIsPositive is a small sanity check that
// the interval this whole cache exists to enforce hasn't been zeroed out
// by accident — the behavioral tests above only prove caching happens
// "for a while," not that the while is actually the intended minute.
func TestCache_Get_minRefetchIntervalIsPositive(t *testing.T) {
	if minRefetchInterval <= 0 {
		t.Fatalf("minRefetchInterval = %v, want a positive duration", minRefetchInterval)
	}
	if minRefetchInterval < time.Second {
		t.Fatalf("minRefetchInterval = %v, suspiciously short for a real-API rate limit", minRefetchInterval)
	}
}
