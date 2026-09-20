package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// cacheHeaderServer serves one event whose ended-ness the test picks.
func cacheHeaderServer(t *testing.T, ended bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"id": "evt-1", "name": "An Event",
			"status": {"started": true, "ended": %t, "currentRound": 2, "numberOfRounds": 5}}`, ended)
	})
	mux.HandleFunc("/events/evt-1/players", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"active": []}`))
	})
	mux.HandleFunc("/events/evt-1/teamplayers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"active": []}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestCacheControl_ConcludedEventIsImmutable is the case worth having.
//
// A concluded event's info cannot change again — it is what the durable
// cache stores permanently — so the browser should never ask twice.
// Before this, nothing in the service set Cache-Control at all and
// every navigation re-fetched it over the network.
func TestCacheControl_ConcludedEventIsImmutable(t *testing.T) {
	e := newBCPTestEcho(bcp.NewClientWithBaseURL(cacheHeaderServer(t, true).URL))

	rec := doBCPRequest(e, http.MethodGet, "/api/events/evt-1")
	got := rec.Header().Get("Cache-Control")

	if !strings.Contains(got, "immutable") {
		t.Errorf("a concluded event answered with Cache-Control %q, want an immutable directive", got)
	}
	if !strings.Contains(got, "private") {
		t.Errorf("Cache-Control %q is missing `private` — these responses are per-session "+
			"and reached with credentials, so a shared cache must never hold one", got)
	}
	if strings.Contains(got, "public") {
		t.Errorf("Cache-Control %q says `public` on a credentialed response", got)
	}
	if want := fmt.Sprintf("max-age=%d", int(bcp.EndedEventTTL.Seconds())); !strings.Contains(got, want) {
		t.Errorf("Cache-Control %q does not carry %q", got, want)
	}
}

// TestCacheControl_LiveEventGetsOnlyTheServersOwnInterval pins the
// bound that makes this safe.
//
// A live event is the case where a stale answer is visible to someone
// standing at a venue. The window it may be held for is exactly the
// interval this service would have answered identically over anyway —
// so the browser skipping the request produces the same bytes, never
// staler ones.
func TestCacheControl_LiveEventGetsOnlyTheServersOwnInterval(t *testing.T) {
	e := newBCPTestEcho(bcp.NewClientWithBaseURL(cacheHeaderServer(t, false).URL))

	rec := doBCPRequest(e, http.MethodGet, "/api/events/evt-1")
	got := rec.Header().Get("Cache-Control")

	if strings.Contains(got, "immutable") {
		t.Fatalf("a live event answered with Cache-Control %q — its current round moves round by round, "+
			"and this is the page someone reads mid-event", got)
	}
	want := fmt.Sprintf("private, max-age=%d", int(bcp.MinRefetchInterval.Seconds()))
	if got != want {
		t.Errorf("Cache-Control = %q, want %q (the server's own refetch interval, no longer)", got, want)
	}
}

// TestCacheControl_RefreshIsNeverStored covers the explicit "check again
// now" path. A refresh button that can be answered from the browser's
// cache is a refresh button that does nothing.
func TestCacheControl_RefreshIsNeverStored(t *testing.T) {
	e := newBCPTestEcho(bcp.NewClientWithBaseURL(cacheHeaderServer(t, true).URL))

	for _, path := range []string{
		"/api/events/evt-1?refresh=true",
		"/api/events/evt-1/players?refresh=true",
	} {
		rec := doBCPRequest(e, http.MethodGet, path)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s answered with Cache-Control %q, want no-store — "+
				"an explicit refresh must reach this service", path, got)
		}
	}
}

// TestCacheControl_AccountRoutesAreNeverStored — /api/me answers "who am
// I and am I approved", which changes the moment an admin approves an
// account and is the first thing every gated page waits on.
func TestCacheControl_AccountRoutesAreNeverStored(t *testing.T) {
	store, cookie := linkedSession(t)
	server, _ := budgetServer(t, nil, 0)
	e := newMeTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	rec := getWithSession(t, e, "/api/me/events", cookie)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("/api/me/events answered with Cache-Control %q, want no-store", got)
	}
}
