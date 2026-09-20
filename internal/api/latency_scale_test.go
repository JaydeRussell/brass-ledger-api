package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// How does /api/me/events cost scale with the size of an account?
//
// The small fixture in latency_budget_test.go can't answer that, and it
// hides the thing that actually scales: both history feeds are
// *paginated*, 100 records per page, and the pages within one feed are
// necessarily sequential — each request needs the previous response's
// cursor. So an account with 350 events pays four sequential BCP round
// trips per feed before any of the per-event work starts.
//
// Answering this with real data would mean crawling BCP for somebody
// with hundreds of events. Instead this synthesises the account, counts
// what the endpoint does, and prices it with the measured constants in
// internal/bcp — the whole point of having a cost model.

// pendingShare is the fraction of an account's events that are
// registered but have no published placing yet, and so need their own
// lookup. Taken from the real account this was measured against: 9
// pending out of 42 (~21%).
const pendingShare = 0.21

// Of those pending events, nearly all are *ended* — the event finished
// and the organiser hasn't published results. Those are durably
// cacheable, so they cost a live BCP call once and a batched read
// thereafter.
//
// Only a handful are genuinely in progress or upcoming, and that number
// doesn't grow with the size of an account: you can only be at one event
// at a time. Those never cache, so they're a live call on every request.
//
// Getting this split right is the difference between a scary curve and a
// real one — modelling every pending event as uncacheable made a
// 500-event account look like 19 seconds.
const liveEventsPerAccount = 2

// scaleStub serves a synthetic account of n events across both history
// feeds, paginating exactly as BCP does.
func scaleStub(t *testing.T, n int) (*httptest.Server, *countingBCP) {
	t.Helper()
	counts := &countingBCP{watched: map[string]bool{}}

	pending := int(float64(n) * pendingShare)
	placed := n - pending

	// page serves records[from:] in 100-record pages, handing back a
	// cursor whenever there's more — the shape the real crawl loops on.
	page := func(w http.ResponseWriter, r *http.Request, total int, record func(i int) string) {
		const perPage = 100
		from := 0
		if k := r.URL.Query().Get("nextKey"); k != "" {
			_, _ = fmt.Sscanf(k, "offset-%d", &from)
		}
		to := from + perPage
		if to > total {
			to = total
		}
		items := make([]string, 0, to-from)
		for i := from; i < to; i++ {
			items = append(items, record(i))
		}
		body := `{"data": [` + strings.Join(items, ",") + `]`
		if to < total {
			body += fmt.Sprintf(`, "nextKey": %q`, fmt.Sprintf("offset-%d", to))
		}
		body += `}`
		_, _ = w.Write([]byte(body))
	}

	mux := http.NewServeMux()

	// Every event the account ever registered for.
	mux.HandleFunc("/players", func(w http.ResponseWriter, r *http.Request) {
		counts.enter("/players")
		defer counts.leave("/players")
		time.Sleep(stubDwell)
		page(w, r, n, func(i int) string {
			return fmt.Sprintf(`{"event": {"id": "evt-%d", "name": "Event %d"}}`, i, i)
		})
	})

	// Only the ones with a published placing.
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, r *http.Request) {
		counts.enter("/eventplacings")
		defer counts.leave("/eventplacings")
		time.Sleep(stubDwell)
		page(w, r, placed, func(i int) string {
			return fmt.Sprintf(
				`{"placing": %d, "event": {"id": "evt-%d", "name": "Event %d", "eventDate": "2024-01-01T00:00:00.000Z"}}`,
				i+1, i, i)
		})
	})

	// The pending ones each need their own lookup. They are the tail of
	// the id range, since the placed ones took the head.
	mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
		counts.enter("/events/:id")
		defer counts.leave("/events/:id")
		time.Sleep(stubDwell)
		id := strings.TrimPrefix(r.URL.Path, "/events/")

		// The last few ids are the genuinely-live ones; everything else
		// has ended and is awaiting results, so it can be cached.
		ended := true
		var idx int
		if _, err := fmt.Sscanf(id, "evt-%d", &idx); err == nil && idx >= n-liveEventsPerAccount {
			ended = false
		}
		_, _ = fmt.Fprintf(w,
			`{"id": %q, "name": %q, "status": {"started": true, "ended": %t}}`, id, id, ended)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, counts
}

// Both slow endpoints, across account sizes. They scale differently and
// it's worth seeing side by side: /api/me/events resolves only the
// events awaiting results, while /api/me/stats resolves *every* event
// with a placing, to classify it. Cold, that's a much bigger set.
func TestLatencyScale_AcrossAccountSizes(t *testing.T) {
	endpoints := []struct {
		name  string
		path  string
		build func(store userStore, client *bcp.Client) http.Handler
	}{
		{"my events", "/api/me/events", func(s userStore, c *bcp.Client) http.Handler { return newMeTestEcho(s, c) }},
		{"stats", "/api/me/stats", func(s userStore, c *bcp.Client) http.Handler { return newStatsTestEcho(s, c) }},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) { scaleAcrossSizes(t, ep.path, ep.build) })
	}
}

func scaleAcrossSizes(t *testing.T, path string, build func(userStore, *bcp.Client) http.Handler) {
	sizes := []int{25, 50, 100, 250, 500}

	type row struct {
		events               int
		coldCalls, warmCalls int
		coldEst, warmEst     time.Duration
	}
	rows := make([]row, 0, len(sizes))

	for _, n := range sizes {
		store, cookie := linkedSession(t)
		server, counts := scaleStub(t, n)
		durable := newCountingDurable()

		measure := func(label string) (int, time.Duration, map[string]int) {
			client := bcp.NewClientWithBaseURL(server.URL)
			client.SetDurableCache(durable)
			getWithSession(t, build(store, client), path, cookie)

			calls, concurrency, byPath := counts.snapshot()
			if concurrency < 1 {
				concurrency = 1
			}
			reads, batches := durable.snapshot()
			est := bcp.EstimatedCost(calls, concurrency, reads, batches)
			t.Logf("%4d events, %-5s → %3d upstream call(s) %v, peak %d concurrent, %d durable read(s), %d batched → est %s",
				n, label, calls, byPath, concurrency, reads, batches, est.Round(time.Millisecond))
			return calls, est, byPath
		}

		// First ever visit: nothing cached anywhere, so both history
		// feeds are crawled page by page.
		coldCalls, coldEst, _ := measure("cold")

		// Then the case that actually recurs — a freshly restarted
		// process reading what the last one left in Postgres. The
		// container sleeps after ten minutes, so this is most visits.
		counts.reset()
		durable.reset()
		warmCalls, warmEst, _ := measure("warm")

		rows = append(rows, row{n, coldCalls, warmCalls, coldEst, warmEst})
	}

	t.Log("")
	t.Logf("  %s", path)
	t.Log("  events    first visit     every visit after   verdict (steady state)")
	for _, r := range rows {
		verdict := "ok"
		if r.warmEst > estimatedPageBudget {
			verdict = fmt.Sprintf("OVER the %s budget", estimatedPageBudget)
		}
		t.Logf("  %6d %14s %21s   %s",
			r.events, r.coldEst.Round(time.Millisecond), r.warmEst.Round(time.Millisecond), verdict)
	}

	first, last := rows[0], rows[len(rows)-1]

	// The steady state must be FLAT, not merely sub-linear. Everything
	// that scales with the account — both history feeds, and every
	// ended-but-unplaced event — is durably cached and batched, so a
	// twenty-fold bigger account should cost the same. Anything that
	// grows here is per-event work that has crept back in.
	if last.warmEst != first.warmEst {
		t.Errorf("steady-state cost changed with account size: %d events → %s, %d events → %s. "+
			"Everything that scales is meant to be batched or cached; something is being done per event.",
			first.events, first.warmEst, last.events, last.warmEst)
	}

	// Every visit after the first must also be within budget at any size.
	for _, r := range rows {
		if r.warmEst > estimatedPageBudget {
			t.Errorf("%d events: steady-state cost estimated at %s, budget is %s",
				r.events, r.warmEst.Round(time.Millisecond), estimatedPageBudget)
		}
	}
}

// Guard against a silently truncated history: maxHistoryPages caps each
// crawl, so past that many pages an account simply stops seeing its own
// events. Worth knowing where that ceiling is.
func TestLatencyScale_PaginationCeiling(t *testing.T) {
	const perPage = 100
	ceiling := 10 * perPage // maxHistoryPages, from internal/bcp/history.go

	store, cookie := linkedSession(t)
	server, counts := scaleStub(t, ceiling+250)
	client := bcp.NewClientWithBaseURL(server.URL)
	getWithSession(t, newMeTestEcho(store, client), "/api/me/events", cookie)

	_, _, byPath := counts.snapshot()
	if byPath["/players"] > 10 {
		t.Errorf("/players fetched %d pages, but maxHistoryPages is 10", byPath["/players"])
	}
	t.Logf("an account past the ceiling crawls %d /players page(s) and %d /eventplacings page(s); "+
		"anything beyond %d events is not returned at all",
		byPath["/players"], byPath["/eventplacings"], ceiling)
}

var _ = json.Marshal
