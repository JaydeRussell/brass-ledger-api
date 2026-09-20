package bcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsGone pins down which statuses count as "this will never
// resolve". The list is deliberately short, and the cases that are
// *excluded* are the ones worth a test: treating any of them as
// permanent would hide real data for goneTTL.
func TestIsGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404 is gone", &StatusError{StatusCode: http.StatusNotFound, URL: "u"}, true},
		{"410 is gone", &StatusError{StatusCode: http.StatusGone, URL: "u"}, true},
		{"403 is not gone", &StatusError{StatusCode: http.StatusForbidden, URL: "u"}, false},
		{"429 is not gone", &StatusError{StatusCode: http.StatusTooManyRequests, URL: "u"}, false},
		{"500 is not gone", &StatusError{StatusCode: http.StatusInternalServerError, URL: "u"}, false},
		{"502 is not gone", &StatusError{StatusCode: http.StatusBadGateway, URL: "u"}, false},
		{"a network error is not gone", errors.New("dial tcp: connection refused"), false},
		{"nil is not gone", nil, false},
		{
			// Callers wrap: fetchEventInfoUncached adds context before
			// returning. errors.As has to see through that, or the
			// negative cache silently never engages.
			name: "a wrapped 404 is still gone",
			err:  fmt.Errorf("fetching event evt-1: %w", &StatusError{StatusCode: http.StatusNotFound, URL: "u"}),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGone(tc.err); got != tc.want {
				t.Errorf("IsGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestStatusError_Message guards the wording, which is user-visible:
// internal/api's bcpError puts err.Error() straight into a 502 body.
func TestStatusError_Message(t *testing.T) {
	err := &StatusError{StatusCode: http.StatusNotFound, URL: "https://example.test/v2/events/evt-1"}
	want := "BCP request to https://example.test/v2/events/evt-1 failed: HTTP 404"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestCache_Gone covers the negative cache: a 404 is asked once and
// replayed, anything else is still retried, and the marker can be
// cleared or expire.
func TestCache_Gone(t *testing.T) {
	notFound := func(key string) error {
		return &StatusError{StatusCode: http.StatusNotFound, URL: "https://example.test/" + key}
	}

	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "a 404 is fetched once and replayed thereafter",
			run: func(t *testing.T) {
				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "", notFound(key)
				})

				for i := range 5 {
					got, err := c.Get(context.Background(), "dead")
					if !IsGone(err) {
						t.Fatalf("Get #%d err = %v, want a gone error", i, err)
					}
					if got != "" {
						t.Errorf("Get #%d value = %q, want the zero value", i, got)
					}
				}
				if calls != 1 {
					t.Errorf("fetch called %d times, want 1 — a 404 must not cost a request per call", calls)
				}
			},
		},
		{
			// The regression that motivated all of this: two dead
			// registrations on /api/me/events cost a BCP round trip on
			// every page load. Different keys must not share a marker.
			name: "each key is remembered separately",
			run: func(t *testing.T) {
				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					if key == "alive" {
						return "ok", nil
					}
					return "", notFound(key)
				})

				for range 3 {
					if _, err := c.Get(context.Background(), "dead-1"); !IsGone(err) {
						t.Fatalf("dead-1 err = %v, want gone", err)
					}
					if _, err := c.Get(context.Background(), "dead-2"); !IsGone(err) {
						t.Fatalf("dead-2 err = %v, want gone", err)
					}
					got, err := c.Get(context.Background(), "alive")
					if err != nil || got != "ok" {
						t.Fatalf("alive = %q, %v; want \"ok\", nil", got, err)
					}
				}
				if calls != 3 {
					t.Errorf("fetch called %d times, want 3 (one per key)", calls)
				}
			},
		},
		{
			// This used to assert the opposite — that a 5xx was retried
			// on every single call — and that was wrong for a service
			// every visitor shares. When BCP had a moment, every page
			// load of every user retried immediately, so the traffic we
			// sent them peaked exactly while they were least able to
			// serve it. The request that failed a second ago was not
			// going to succeed now.
			//
			// What has to stay true is that a transient failure never
			// becomes a permanent one, which is why this backs off for
			// upstreamFailureBackoff rather than goneTTL's hour.
			name: "a 500 backs off briefly rather than being retried on every call",
			run: func(t *testing.T) {
				restore := upstreamFailureBackoff
				upstreamFailureBackoff = 20 * time.Millisecond
				t.Cleanup(func() { upstreamFailureBackoff = restore })

				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "", &StatusError{StatusCode: http.StatusInternalServerError, URL: "u"}
				})

				for range 3 {
					if _, err := c.Get(context.Background(), "flaky"); err == nil {
						t.Fatal("Get returned nil error, want the upstream failure")
					}
				}
				if calls != 1 {
					t.Errorf("fetch called %d times inside the backoff, want 1 — "+
						"hammering an API that is already failing is the opposite of respecting it", calls)
				}

				time.Sleep(40 * time.Millisecond)
				if _, err := c.Get(context.Background(), "flaky"); err == nil {
					t.Fatal("Get returned nil error, want the upstream failure")
				}
				if calls != 2 {
					t.Errorf("fetch called %d times after the backoff elapsed, want 2 — "+
						"a 5xx must never become permanent", calls)
				}
			},
		},
		{
			// The escape hatch that makes the backoff acceptable: a
			// person who just watched something fail and pressed the
			// refresh button gets a real request, not a replayed error.
			// Same rule as everywhere else here — automatic traffic is
			// rate-limited, an explicit user action never is.
			name: "an explicit refresh bypasses the failure backoff",
			run: func(t *testing.T) {
				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					n := atomic.AddInt32(&calls, 1)
					if n == 1 {
						return "", &StatusError{StatusCode: http.StatusBadGateway, URL: "u"}
					}
					return "recovered", nil
				})

				if _, err := c.Get(context.Background(), "flaky"); err == nil {
					t.Fatal("first Get returned nil error, want the upstream failure")
				}
				if !c.Invalidate("flaky") {
					t.Fatal("Invalidate reported nothing to clear after a failed fetch")
				}
				got, err := c.Get(context.Background(), "flaky")
				if err != nil {
					t.Fatalf("Get after an explicit refresh returned %v, want a real retry", err)
				}
				if got != "recovered" {
					t.Errorf("Get after refresh = %q, want %q", got, "recovered")
				}
			},
		},
		{
			name: "a transport failure backs off the same way a 5xx does",
			run: func(t *testing.T) {
				restore := upstreamFailureBackoff
				upstreamFailureBackoff = 20 * time.Millisecond
				t.Cleanup(func() { upstreamFailureBackoff = restore })

				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "", errors.New("dial tcp: connection refused")
				})

				for range 3 {
					if _, err := c.Get(context.Background(), "unreachable"); err == nil {
						t.Fatal("Get returned nil error, want the transport failure")
					}
				}
				if calls != 1 {
					t.Errorf("fetch called %d times inside the backoff, want 1", calls)
				}

				time.Sleep(40 * time.Millisecond)
				if _, err := c.Get(context.Background(), "unreachable"); err == nil {
					t.Fatal("Get returned nil error, want the transport failure")
				}
				if calls != 2 {
					t.Errorf("fetch called %d times after the backoff, want 2 — "+
						"a host that was unreachable a moment ago may well be reachable now", calls)
				}
			},
		},
		{
			name: "the marker expires after goneTTL",
			run: func(t *testing.T) {
				restore := goneTTL
				goneTTL = 20 * time.Millisecond
				t.Cleanup(func() { goneTTL = restore })

				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "", notFound(key)
				})

				if _, err := c.Get(context.Background(), "dead"); !IsGone(err) {
					t.Fatalf("first Get err = %v, want gone", err)
				}
				if _, err := c.Get(context.Background(), "dead"); !IsGone(err) {
					t.Fatalf("second Get err = %v, want gone", err)
				}
				if calls != 1 {
					t.Fatalf("fetch called %d times before expiry, want 1", calls)
				}

				time.Sleep(30 * time.Millisecond)
				if _, err := c.Get(context.Background(), "dead"); !IsGone(err) {
					t.Fatalf("Get after expiry err = %v, want gone", err)
				}
				if calls != 2 {
					t.Errorf("fetch called %d times after expiry, want 2 — the marker must not be permanent", calls)
				}
			},
		},
		{
			name: "an event that comes back is served again",
			run: func(t *testing.T) {
				restore := goneTTL
				goneTTL = 20 * time.Millisecond
				t.Cleanup(func() { goneTTL = restore })

				var alive atomic.Bool
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					if alive.Load() {
						return "back", nil
					}
					return "", notFound(key)
				})

				if _, err := c.Get(context.Background(), "evt"); !IsGone(err) {
					t.Fatalf("first Get err = %v, want gone", err)
				}
				alive.Store(true)
				time.Sleep(30 * time.Millisecond)

				got, err := c.Get(context.Background(), "evt")
				if err != nil || got != "back" {
					t.Fatalf("Get after recovery = %q, %v; want \"back\", nil", got, err)
				}
				// And the success must have cleared the marker, not just
				// outlived it — otherwise the next 404-free call would
				// still replay the stale error once the value expires.
				c.mu.Lock()
				_, stillMarked := c.gone["evt"]
				c.mu.Unlock()
				if stillMarked {
					t.Error("a successful fetch left the gone marker in place")
				}
			},
		},
		{
			name: "Invalidate clears the marker",
			run: func(t *testing.T) {
				var calls int32
				c := NewCache(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "", notFound(key)
				})

				if _, err := c.Get(context.Background(), "dead"); !IsGone(err) {
					t.Fatalf("first Get err = %v, want gone", err)
				}
				if !c.Invalidate("dead") {
					t.Fatal("Invalidate reported no change for a gone key")
				}
				if _, err := c.Get(context.Background(), "dead"); !IsGone(err) {
					t.Fatalf("Get after Invalidate err = %v, want gone", err)
				}
				if calls != 2 {
					t.Errorf("fetch called %d times, want 2 — an explicit refresh must re-ask", calls)
				}
			},
		},
		{
			// Put seeds a value the durable cache already had, which is
			// proof the key is not gone. The marker has to go with it,
			// and outliving the seeded entry is the case that catches a
			// missing clear: a live entry is checked before the marker,
			// so while it's fresh the difference is invisible.
			name: "Put clears the marker, not just shadows it",
			run: func(t *testing.T) {
				var calls int32
				c := NewCacheWithTTL(func(ctx context.Context, key string) (string, error) {
					atomic.AddInt32(&calls, 1)
					return "", notFound(key)
				}, 20*time.Millisecond)

				if _, err := c.Get(context.Background(), "dead"); !IsGone(err) {
					t.Fatalf("first Get err = %v, want gone", err)
				}
				c.Put("dead", "seeded", time.Now())

				got, err := c.Get(context.Background(), "dead")
				if err != nil || got != "seeded" {
					t.Fatalf("Get after Put = %q, %v; want \"seeded\", nil", got, err)
				}
				if calls != 1 {
					t.Errorf("fetch called %d times, want 1 — Put must satisfy the read", calls)
				}

				// Once the seeded entry ages out, the key must go back
				// to BCP — not replay an hour-old 404 the Put disproved.
				time.Sleep(30 * time.Millisecond)
				if _, err := c.Get(context.Background(), "dead"); !IsGone(err) {
					t.Fatalf("Get after the entry expired err = %v, want a fresh gone error", err)
				}
				if calls != 2 {
					t.Errorf("fetch called %d times after the seeded entry expired, want 2", calls)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t) })
	}
}
