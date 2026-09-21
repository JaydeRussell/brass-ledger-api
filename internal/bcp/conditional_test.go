package bcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// etagServer answers with an ETag and honours If-None-Match, the way
// BCP does — verified against the real API on 2026-09-20, where
// re-requesting a roster with its own ETag came back 304 with zero
// bytes instead of 2,144.
func etagServer(t *testing.T, etag, body string) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var requests, notModified atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &requests, &notModified
}

const etagEventBody = `{"id": "evt-1", "name": "Conditional Cup", "status": {"started": true, "ended": false}}`

// TestConditionalRequest_RevalidatesWithoutRefetchingTheBody is the
// whole point: the same number of requests to BCP, almost none of the
// bytes.
//
// It matters exactly where the durable cache cannot help — an event in
// progress, its standings, a player's history. Those are refetched on a
// timer forever because their data genuinely moves, and most of the
// time it hasn't moved yet.
func TestConditionalRequest_RevalidatesWithoutRefetchingTheBody(t *testing.T) {
	server, requests, notModified := etagServer(t, `W/"abc123"`, etagEventBody)
	c := NewClientWithBaseURL(server.URL)

	first, err := c.fetchEventInfoUncached(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if first.Name != "Conditional Cup" {
		t.Fatalf("first fetch returned %q", first.Name)
	}

	second, err := c.fetchEventInfoUncached(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	if notModified.Load() != 1 {
		t.Errorf("the second request wasn't conditional (%d of %d answered 304) — without "+
			"If-None-Match every refetch pays for a payload BCP already told us it could skip",
			notModified.Load(), requests.Load())
	}
	if second.Name != "Conditional Cup" {
		t.Errorf("a 304 resolved to %+v; it carries no body, so the stored one has to answer it", second)
	}
}

// TestConditionalRequest_NoticesRealChanges — revalidation must not
// become a way of never seeing an update.
func TestConditionalRequest_NoticesRealChanges(t *testing.T) {
	etag := `W/"v1"`
	body := etagEventBody
	var requests atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClientWithBaseURL(server.URL)
	if _, err := c.fetchEventInfoUncached(context.Background(), "evt-1"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	// The event moves on: new round, new ETag.
	etag = `W/"v2"`
	body = `{"id": "evt-1", "name": "Conditional Cup", "status": {"started": true, "ended": true}}`

	updated, err := c.fetchEventInfoUncached(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("post-change fetch: %v", err)
	}
	if !updated.Ended {
		t.Error("a changed response was not picked up — a stale validator must never suppress a real update")
	}

	// And the new validator is the one now in play.
	third, err := c.fetchEventInfoUncached(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("third fetch: %v", err)
	}
	if !third.Ended {
		t.Error("the refreshed body was not stored, so the next 304 answered with the old one")
	}
}

// TestConditionalRequest_UnvalidatableResponsesAreNotStored — a
// response with no ETag can never be revalidated, so holding its body
// is pure memory cost.
func TestConditionalRequest_UnvalidatableResponsesAreNotStored(t *testing.T) {
	var sentValidator atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			sentValidator.Store(true)
		}
		_, _ = w.Write([]byte(etagEventBody))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClientWithBaseURL(server.URL)
	for i := range 2 {
		if _, err := c.fetchEventInfoUncached(context.Background(), "evt-1"); err != nil {
			t.Fatalf("fetch %d: %v", i+1, err)
		}
	}

	if sentValidator.Load() {
		t.Error("sent If-None-Match for a response that never carried an ETag")
	}
	if _, stored := c.conditional.body(fmt.Sprintf("%s/events/evt-1?role=true", server.URL)); stored {
		t.Error("stored a body with no ETag — it could never be revalidated, so it is memory held for nothing")
	}
}

// TestTimings_SeparatesFetchesFromRevalidations pins the split the
// costs endpoint reports.
//
// Conditional requests mean a growing share of calls to BCP are 304s
// carrying nothing, and they are much cheaper than a real fetch.
// Folding both into one figure would quietly drag the observed
// round-trip cost below what a real fetch costs — which is precisely
// the number cost.go's RoundTripCost models, so the one measurement
// meant to keep that constant honest would be the one misleading it.
func TestTimings_SeparatesFetchesFromRevalidations(t *testing.T) {
	server, requests, notModified := etagServer(t, `W/"abc123"`, etagEventBody)
	c := NewClientWithBaseURL(server.URL)

	// fetchEventInfoUncached, not FetchEventInfo: the cache would serve
	// the second call from memory and no second exchange would happen at
	// all. Invalidate is no help either — it throttles a repeat inside
	// minManualInvalidateInterval, which is exactly this window.
	for range 3 {
		if _, err := c.fetchEventInfoUncached(context.Background(), "evt-1"); err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}

	fetches, revalidations := c.Timings()

	if fetches.Count != 1 {
		t.Errorf("recorded %d full fetches, want 1 (the first call, before there was a validator)",
			fetches.Count)
	}
	if revalidations.Count != 2 {
		t.Errorf("recorded %d revalidations, want 2", revalidations.Count)
	}
	if total := fetches.Count + revalidations.Count; total != int64(requests.Load()) {
		t.Errorf("recorded %d exchanges against %d served", total, requests.Load())
	}
	if notModified.Load() != 2 {
		t.Errorf("server answered %d of them 304, want 2", notModified.Load())
	}
}

// TestTimings_FailuresAreNotRecorded — a connection refused in a
// millisecond is not evidence about how long BCP takes to answer, and
// averaging it in would make things look faster the worse they got.
func TestTimings_FailuresAreNotRecorded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClientWithBaseURL(server.URL)
	if _, err := c.fetchEventInfoUncached(context.Background(), "evt-1"); err == nil {
		t.Fatal("expected the 502 to surface as an error")
	}

	fetches, revalidations := c.Timings()
	if fetches.Count != 0 || revalidations.Count != 0 {
		t.Errorf("a failed exchange was recorded (%d fetches, %d revalidations); it says nothing "+
			"about how long a successful one takes", fetches.Count, revalidations.Count)
	}
}
