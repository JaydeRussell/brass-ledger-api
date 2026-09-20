package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// Budgets for the endpoints behind the pages people actually wait on.
//
// CLAUDE.md treats a page over two seconds as a bug. This file is how
// that survives contact with the next change — but it asserts *shape*,
// not milliseconds, and the distinction matters:
//
//   - CI has no Best Coast Pairings data and must never call BCP (it's
//     an unofficial API we have no agreement with). So a wall-clock
//     budget here would be timing a stub server on a shared runner:
//     sub-millisecond, noisy, and measuring nothing real.
//   - Every latency regression this project has actually had was
//     structural — a batch that quietly stopped batching, two
//     independent fetches left in series, a client waiting on a request
//     it didn't need. Those show up as *round-trip counts*, which are
//     exactly reproducible with the httptest stub the other tests
//     already use.
//
// So: count the upstream requests, and check the ones that should
// overlap actually do. Real wall-clock numbers come from
// scripts/latency-map.sh against a deployment, where they mean
// something.
const (
	// An account with events awaiting results resolves each of them.
	// The fixture below has three such events plus the two history
	// feeds, so anything beyond this means something stopped batching
	// or started re-fetching.
	myEventsUpstreamBudget = 5

	// /api/me/stats reads one history feed and resolves what it needs
	// from the durable cache in a batch.
	myStatsUpstreamBudget = 3

	// What one page's endpoints may be estimated to cost in production.
	//
	// The rule is two seconds (CLAUDE.md). This asserts against 1.4s
	// instead, on purpose: bcp.EstimatedCost models only round trips and
	// comes out roughly 25% optimistic when things are already fast (see
	// its calibration table), so it's a lower bound. Asserting at 2s
	// would let a page that really takes 2.4s pass. The headroom is that
	// error margin, not slack.
	estimatedPageBudget = 1400 * time.Millisecond

	// What one event-page load may cost upstream.
	//
	// The event page is the heaviest thing in the app and the one
	// actually used mid-event, on a phone, on venue wifi — and until
	// now it had no budget at all, while /api/me/events and
	// /api/me/stats each had one. The fixture below is one concluded
	// event with one league and three players, which the frontend
	// loads as: event info, roster, the ITC league lookup, one round of
	// pairings, then one ITC ranking per player.
	//
	// That comes to 8: 1 event info (v2), 2 roster (/players and
	// /teamplayers), 1 league, 1 pairings, 3 ITC rankings. The number
	// to watch is the last one — it is one call per player by
	// necessity, not by choice (BCP's /placings honours exactly one
	// userId[]; repeated, comma-joined and userIds[] forms were all
	// tested against the real API on 2026-09-20 and none of them
	// filter). So this budget scales with roster size, and a team
	// event's two full rosters are the worst case in the app.
	eventPageUpstreamBudget = 8

	// Per-key durable reads allowed for one /api/me/events on a warm
	// durable cache. The two history feeds read theirs individually by
	// design (two keys, not a set). What this bounds is anything *per
	// event* on top of the batched prewarm — the fixture has three
	// pending events, so an N+1 would push this straight past the line.
	durablePerKeyReadBudget = 3
)

// barrier releases every waiter once n of them have arrived — but gives
// up after a timeout instead of blocking forever.
//
// That matters: a sync.WaitGroup gate would deadlock a serial
// implementation, which does fail the test, but as a 40-second goroutine
// dump from the test binary's own timeout rather than as a readable
// assertion. Timing out and letting the handler answer means the
// concurrency assertion is what fails, and it can say why.
type barrier struct {
	n    int
	mu   sync.Mutex
	seen int
	open chan struct{}
}

func newBarrier(n int) *barrier {
	return &barrier{n: n, open: make(chan struct{})}
}

func (b *barrier) arrive(timeout time.Duration) {
	b.mu.Lock()
	b.seen++
	if b.seen >= b.n {
		select {
		case <-b.open:
		default:
			close(b.open)
		}
	}
	b.mu.Unlock()

	select {
	case <-b.open:
	case <-time.After(timeout):
		// Never reached them all — whatever is being asserted about
		// concurrency is about to fail, with a better message.
	}
}

// stubDwell is how long each stubbed upstream response is held. Long
// enough that concurrent callers demonstrably overlap, short enough that
// the suite stays fast.
const stubDwell = 5 * time.Millisecond

// barrierTimeout is how long a gated handler waits for its peers before
// giving up and answering anyway.
const barrierTimeout = 2 * time.Second

// countingBCP is the stub the budgets are measured against. It records
// every upstream request and, crucially, the high-water mark of
// concurrent ones — which is what tells a parallel fetch apart from a
// serial one without timing anything.
type countingBCP struct {
	mu        sync.Mutex
	byPath    map[string]int
	total     int
	inFlight  int
	maxInFlig int

	// Concurrency among a named subset of paths. Global peak is not
	// enough: serialising the per-event lookups still leaves the two
	// history crawls overlapping, so a global peak of 2 would hide it —
	// and vice versa. Each test names the group it's making a claim
	// about.
	watched         map[string]bool
	watchedInFlight int
	watchedMax      int
}

func (c *countingBCP) enter(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byPath == nil {
		c.byPath = map[string]int{}
	}
	c.byPath[path]++
	c.total++
	c.inFlight++
	if c.inFlight > c.maxInFlig {
		c.maxInFlig = c.inFlight
	}
	if c.watched[path] {
		c.watchedInFlight++
		if c.watchedInFlight > c.watchedMax {
			c.watchedMax = c.watchedInFlight
		}
	}
}

func (c *countingBCP) leave(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
	if c.watched[path] {
		c.watchedInFlight--
	}
}

// watchedPeak is the most simultaneous requests seen among the paths the
// test asked to watch.
func (c *countingBCP) watchedPeak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watchedMax
}

// reset zeroes the counters, so a test can populate caches with one
// client and then measure only what a second, freshly started one does.
func (c *countingBCP) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byPath = map[string]int{}
	c.total, c.maxInFlig = 0, 0
}

func (c *countingBCP) snapshot() (total, maxConcurrent int, byPath map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.byPath))
	for k, v := range c.byPath {
		out[k] = v
	}
	return c.total, c.maxInFlig, out
}

// budgetServer serves an account with three events awaiting results —
// the case that made My Events slow, since only *ended* events are
// durably cached and everything else is a live lookup.
//
// Each handler blocks on `release` until every expected caller has
// arrived, so a genuinely serial implementation deadlocks itself into
// the timeout rather than quietly passing. That's the point: this can
// detect "these ran one after another" without measuring time.
func budgetServer(t *testing.T, watch []string, barrierSize int) (*httptest.Server, *countingBCP) {
	t.Helper()
	counts := &countingBCP{watched: map[string]bool{}}
	for _, p := range watch {
		counts.watched[p] = true
	}
	var gate *barrier
	if barrierSize > 0 {
		gate = newBarrier(barrierSize)
	}
	gateFor := func(path string) *barrier {
		if gate != nil && counts.watched[path] {
			return gate
		}
		return nil
	}

	handle := func(path string, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counts.enter(path)
			defer counts.leave(path)
			var gate *barrier
			if gateFor != nil {
				gate = gateFor(path)
			}
			if gate != nil {
				gate.arrive(barrierTimeout)
			} else {
				// Without a gate the stub would answer instantly, and
				// goroutines that genuinely run in parallel would rarely
				// overlap on the clock — so the observed peak would read
				// as 1 and a concurrent implementation would be
				// indistinguishable from a serial one. A short dwell
				// makes the overlap real and therefore measurable.
				time.Sleep(stubDwell)
			}
			_, _ = w.Write([]byte(body))
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/players", handle("/players", `{"data": [
		{"event": {"id": "evt-placed", "name": "Already Placed"}},
		{"event": {"id": "evt-a", "name": "Awaiting A"}},
		{"event": {"id": "evt-b", "name": "Awaiting B"}},
		{"event": {"id": "evt-c", "name": "Awaiting C"}}
	]}`))
	mux.HandleFunc("/eventplacings", handle("/eventplacings", `{"data": [
		{"placing": 4, "event": {"id": "evt-placed", "name": "Already Placed", "eventDate": "2024-01-01T00:00:00.000Z"}}
	]}`))
	for _, id := range []string{"evt-a", "evt-b", "evt-c"} {
		body := fmt.Sprintf(`{"id": %q, "name": %q, "status": {"started": true, "ended": true}}`, id, id)
		mux.HandleFunc("/events/"+id, handle("/events/:id", body))
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, counts
}

// countingDurable is a bcp.DurableCache that records how it's read, so
// a budget test can tell "one batched lookup" from "one lookup per id"
// — the shape of every N+1 this endpoint has had.
type countingDurable struct {
	mu       sync.Mutex
	rows     map[string][]byte
	gets     int // per-key reads
	getManys int // batched reads
}

func newCountingDurable() *countingDurable {
	return &countingDurable{rows: map[string][]byte{}}
}

func (d *countingDurable) Get(_ context.Context, key string, _ int, dest any) (bool, error) {
	d.mu.Lock()
	raw, ok := d.rows[key]
	d.gets++
	d.mu.Unlock()
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, dest)
}

func (d *countingDurable) GetFresh(_ context.Context, key string, _ int, _ time.Duration, dest any) (bool, time.Time, error) {
	d.mu.Lock()
	raw, ok := d.rows[key]
	d.gets++
	d.mu.Unlock()
	if !ok {
		return false, time.Time{}, nil
	}
	return true, time.Now(), json.Unmarshal(raw, dest)
}

func (d *countingDurable) GetMany(_ context.Context, keys []string, _ int) (map[string]bcp.DurableRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.getManys++
	found := map[string]bcp.DurableRow{}
	for _, k := range keys {
		if raw, ok := d.rows[k]; ok {
			found[k] = bcp.DurableRow{Data: json.RawMessage(raw), CachedAt: time.Now()}
		}
	}
	return found, nil
}

func (d *countingDurable) Set(_ context.Context, key string, _ int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.rows[key] = raw
	d.mu.Unlock()
	return nil
}

func (d *countingDurable) Delete(_ context.Context, key string) error {
	d.mu.Lock()
	delete(d.rows, key)
	d.mu.Unlock()
	return nil
}

// reset zeroes the counters while keeping the stored rows — so a test
// can populate the cache with one client, then measure only what a
// second, freshly-started one does with it.
func (d *countingDurable) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gets, d.getManys = 0, 0
}

func (d *countingDurable) snapshot() (gets, getManys int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gets, d.getManys
}

func linkedSession(t *testing.T) (*fakeUserStore, *http.Cookie) {
	t.Helper()
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}
	return store, cookie
}

func getWithSession(t *testing.T, e interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body: %s)", path, rec.Code, rec.Body.String())
	}
	return rec
}

func TestLatencyBudget_MyEventsStaysWithinItsUpstreamBudget(t *testing.T) {
	store, cookie := linkedSession(t)
	server, counts := budgetServer(t, nil, 0)
	e := newMeTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	getWithSession(t, e, "/api/me/events", cookie)

	total, _, byPath := counts.snapshot()
	if total > myEventsUpstreamBudget {
		t.Errorf("GET /api/me/events made %d upstream requests, budget is %d.\nBy path: %v\n"+
			"Something is fetching one at a time that used to be batched, or re-fetching what's already cached.",
			total, myEventsUpstreamBudget, byPath)
	}
	// Each history feed is a paginated crawl — reading either one twice
	// in a single request means a cache isn't being consulted.
	for _, p := range []string{"/players", "/eventplacings"} {
		if byPath[p] > 1 {
			t.Errorf("%s fetched %d times in one request, want 1", p, byPath[p])
		}
	}
}

func TestLatencyBudget_MyStatsStaysWithinItsUpstreamBudget(t *testing.T) {
	store, cookie := linkedSession(t)
	server, counts := budgetServer(t, nil, 0)
	e := newStatsTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	getWithSession(t, e, "/api/me/stats", cookie)

	total, _, byPath := counts.snapshot()
	if total > myStatsUpstreamBudget {
		t.Errorf("GET /api/me/stats made %d upstream requests, budget is %d.\nBy path: %v",
			total, myStatsUpstreamBudget, byPath)
	}
}

// A second identical request, inside every TTL, must not touch the
// network at all.
//
// Note what this does *not* cover: the v0.19.2 regression, where a
// batch-load skipped anything the cache had ever held rather than
// anything still fresh. Reproducing that needs entries that are
// stale-but-present, which needs control of the clock — so it's guarded
// where that control exists, in internal/bcp's
// TestPrewarmEventInfo_StillBatchesOnceTheInMemoryEntriesGoStale. This
// one guards the neighbouring property: nothing refetches early.
func TestLatencyBudget_ASecondRequestInsideTheTTLIsFree(t *testing.T) {
	store, cookie := linkedSession(t)
	server, counts := budgetServer(t, nil, 0)
	e := newMeTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	getWithSession(t, e, "/api/me/events", cookie)
	afterFirst, _, _ := counts.snapshot()

	getWithSession(t, e, "/api/me/events", cookie)
	afterSecond, _, byPath := counts.snapshot()

	if afterSecond != afterFirst {
		t.Errorf("a second /api/me/events inside the TTL made %d more upstream requests, want 0.\nBy path: %v\n"+
			"Every cached lookup should have been served from memory.",
			afterSecond-afterFirst, byPath)
	}
}

// The two history feeds are independent, and each is a paginated crawl.
// Run in series this endpoint costs their sum; run together, the slower
// of the two. The gate makes serial execution fail by timeout rather
// than pass quietly.
func TestLatencyBudget_TheTwoHistoryCrawlsOverlap(t *testing.T) {
	store, cookie := linkedSession(t)

	// Both crawls must be in flight before either may answer.
	server, counts := budgetServer(t, []string{"/players", "/eventplacings"}, 2)
	e := newMeTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	done := make(chan struct{})
	go func() {
		defer close(done)
		getWithSession(t, e, "/api/me/events", cookie)
	}()

	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("GET /api/me/events did not finish: the placing-history and registration crawls " +
			"are running one after the other, so neither could reach the gate the other was waiting on")
	}

	if peak := counts.watchedPeak(); peak < 2 {
		t.Errorf("the two history crawls peaked at %d concurrent, want 2 — they are independent "+
			"and each is a paginated crawl, so running them in series doubles this endpoint's cost.", peak)
	}
}

// The per-event lookups for registrations awaiting results are the other
// thing that used to be a queue. This asserts BOTH ends, because only
// checking the upper bound is vacuous — tightening the bound to 1 (i.e.
// re-serialising them) would sail through a bound-only check.
//
// The gate makes serialisation fail by timeout: all three lookups must
// be in flight simultaneously before any of them may answer.
func TestLatencyBudget_PendingEventLookupsOverlapButStayBounded(t *testing.T) {
	store, cookie := linkedSession(t)

	// A literal 2, deliberately NOT maxConcurrentEventInfo: deriving the
	// barrier from the constant under test makes the check circular —
	// serialising the code to 1 would simply shrink the barrier to 1 and
	// pass. Two is the weakest claim worth making ("more than one at a
	// time") and it holds for any bound above 1.
	server, counts := budgetServer(t, []string{"/events/:id"}, 2)
	e := newMeTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	done := make(chan struct{})
	go func() {
		defer close(done)
		getWithSession(t, e, "/api/me/events", cookie)
	}()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("GET /api/me/events did not finish: the per-event lookups are running one after " +
			"another, so they could never all reach the gate together. maxConcurrentEventInfo is " +
			"what makes them overlap.")
	}

	maxConcurrent := counts.watchedPeak()
	if maxConcurrent < 2 {
		t.Errorf("the per-event lookups peaked at %d concurrent, want at least 2 — resolving them "+
			"one at a time is what made this endpoint take seconds.", maxConcurrent)
	}
	if maxConcurrent > maxConcurrentEventInfo {
		t.Errorf("the per-event lookups peaked at %d concurrent, exceeding the intended bound of %d. "+
			"An unbounded fan-out against BCP is exactly what maxConcurrentEventInfo exists to "+
			"prevent — see CLAUDE.md.",
			maxConcurrent, maxConcurrentEventInfo)
	}
}

// timeoutAfter bounds the gated tests above. Generous, because it should
// only ever be reached when something genuinely runs in series — this is
// a failure signal, not a performance measurement.
func timeoutAfter() <-chan time.Time {
	return time.After(10 * time.Second)
}

// The production shape this guards: a container that has just restarted,
// reading a durable cache that a previous process already populated.
// The pending events must come back in ONE batched query — not one
// sequential round trip each, which is what made a cold /api/me/stats
// take nearly three seconds.
//
// Counted rather than timed, so it holds on any machine.
func TestLatencyBudget_DurableCacheIsReadInOneBatchNotPerEvent(t *testing.T) {
	store, cookie := linkedSession(t)
	server, _ := budgetServer(t, nil, 0)
	durable := newCountingDurable()

	// First process: populates the durable cache as it resolves things.
	first := bcp.NewClientWithBaseURL(server.URL)
	first.SetDurableCache(durable)
	getWithSession(t, newMeTestEcho(store, first), "/api/me/events", cookie)

	// Second process — fresh in-memory cache, same Postgres. This is
	// every visit after the container has slept.
	durable.reset()
	second := bcp.NewClientWithBaseURL(server.URL)
	second.SetDurableCache(durable)
	getWithSession(t, newMeTestEcho(store, second), "/api/me/events", cookie)

	gets, getManys := durable.snapshot()
	if getManys < 1 {
		t.Errorf("durable GetMany called %d times, want at least 1 — the pending events should be "+
			"prewarmed in a single query. If the prewarm call was removed, or skipped the set, every "+
			"lookup falls back to its own round trip.", getManys)
	}
	if gets > durablePerKeyReadBudget {
		t.Errorf("durable per-key reads = %d, budget is %d (batched reads: %d).\n"+
			"That is the N+1 shape: one sequential read per event instead of one query for the set.",
			gets, durablePerKeyReadBudget, getManys)
	}
}

// The rule this whole file defends, asserted in the units the rule is
// written in.
//
// Counting round trips is what's reproducible offline; multiplying by
// what a round trip costs in production (internal/bcp's measured
// constants) turns that into a number comparable to "two seconds". A
// regression then fails as "this would take 3.1s in production" rather
// than "the count went from 3 to 12" — the same defect, said in a way
// that's actionable without knowing the internals.
//
// Fixture: an account with three events awaiting results, which is the
// shape that made these pages slow.
func TestLatencyBudget_EstimatedProductionCostOfEachPage(t *testing.T) {
	cases := []struct {
		page     string
		endpoint string
		build    func(store userStore, client *bcp.Client) http.Handler
	}{
		{"My Events / Calendar", "/api/me/events", func(s userStore, c *bcp.Client) http.Handler { return newMeTestEcho(s, c) }},
		{"Player Stats", "/api/me/stats", func(s userStore, c *bcp.Client) http.Handler { return newStatsTestEcho(s, c) }},
	}

	for _, tc := range cases {
		t.Run(tc.page, func(t *testing.T) {
			store, cookie := linkedSession(t)
			server, counts := budgetServer(t, nil, 0)
			durable := newCountingDurable()

			// Populate, then measure a freshly started process reading
			// what the previous one left — every visit after the
			// container has slept.
			warm := bcp.NewClientWithBaseURL(server.URL)
			warm.SetDurableCache(durable)
			getWithSession(t, tc.build(store, warm), tc.endpoint, cookie)

			durable.reset()
			counts.reset() // charge the cold run for its own work only
			cold := bcp.NewClientWithBaseURL(server.URL)
			cold.SetDurableCache(durable)
			getWithSession(t, tc.build(store, cold), tc.endpoint, cookie)

			reads, batches := durable.snapshot()
			// Observed, not assumed: if someone re-serialises these the
			// peak drops to 1 and the estimate rises accordingly, which
			// is the entire point.
			total, concurrency, byPath := counts.snapshot()
			if concurrency < 1 {
				concurrency = 1
			}
			est := bcp.EstimatedCost(total, concurrency, reads, batches)
			t.Logf("%s (%s): estimated %s in production — %d upstream call(s) at up to %d concurrent, "+
				"%d per-key durable read(s), %d batched. By path: %v",
				tc.page, tc.endpoint, est.Round(time.Millisecond), total, concurrency, reads, batches, byPath)

			if est > estimatedPageBudget {
				t.Errorf("%s would take about %s in production, budget is %s (the rule is 2s, "+
					"less the model's ~25%% optimism — see bcp.EstimatedCost).\n"+
					"Observed on a freshly started process: %d upstream call(s) at %d concurrent, "+
					"%d per-key durable read(s), %d batched.\n"+
					"By path: %v\n"+
					"Something is being fetched one at a time that should be batched or overlapped.",
					tc.page, est.Round(time.Millisecond), estimatedPageBudget,
					total, concurrency, reads, batches, byPath)
			}
		})
	}
}

// A BCP registration can outlive the event it points at: the organizer
// deletes the event, the player's registration list keeps naming it, and
// /api/me/events resolves every not-yet-concluded registration through
// FetchEventInfo. The failure is tolerated and the event is dropped from
// the response, so nothing looks broken — which is exactly why this went
// unnoticed. Measured in production on 2026-09-20: two such
// registrations on one account cost a real BCP round trip on *every*
// request, warm, putting /api/me/events at 731ms against /api/me/stats'
// 231ms.
//
// One lookup per dead event per process, not per request. Anything else
// is both a latency bug and a standing request to BCP for something they
// have already said does not exist — see CLAUDE.md's "Be respectful of
// BCP's API".
func TestLatencyBudget_ADeletedEventIsLookedUpOncePerProcessNotPerRequest(t *testing.T) {
	counts := &countingBCP{watched: map[string]bool{}}
	handle := func(path, body string, status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counts.enter(path)
			defer counts.leave(path)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/players", handle("/players", `{"data": [
		{"event": {"id": "evt-placed", "name": "Already Placed"}},
		{"event": {"id": "evt-deleted", "name": "Deleted By Organizer"}}
	]}`, http.StatusOK))
	mux.HandleFunc("/eventplacings", handle("/eventplacings", `{"data": [
		{"placing": 4, "event": {"id": "evt-placed", "name": "Already Placed", "eventDate": "2024-01-01T00:00:00.000Z"}}
	]}`, http.StatusOK))
	mux.HandleFunc("/events/evt-deleted", handle("/events/:id", `{"message": "not found"}`, http.StatusNotFound))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	store, cookie := linkedSession(t)
	e := newMeTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	var last *httptest.ResponseRecorder
	for range 3 {
		last = getWithSession(t, e, "/api/me/events", cookie)
	}

	_, _, byPath := counts.snapshot()
	if got := byPath["/events/:id"]; got != 1 {
		t.Errorf("a deleted event was looked up %d times across 3 requests, want 1.\nBy path: %v\n"+
			"A 404 is permanent — re-asking it every request is a per-page-load tax on data that will never arrive.",
			got, byPath)
	}

	// And the dead event still must not surface. The negative cache
	// changes how often we ask, never what the user sees.
	var body struct {
		Past    []myEvent `json:"past"`
		Present []myEvent `json:"present"`
		Future  []myEvent `json:"future"`
	}
	if err := json.Unmarshal(last.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	for _, group := range [][]myEvent{body.Past, body.Present, body.Future} {
		for _, ev := range group {
			if ev.EventID == "evt-deleted" {
				t.Errorf("a deleted event appeared in the response as %+v, want it omitted", ev)
			}
		}
	}
	if len(body.Past) != 1 || body.Past[0].EventID != "evt-placed" {
		t.Errorf("Past = %+v, want just evt-placed — the real event must still be served", body.Past)
	}
}

// TestLatencyBudget_ADeadLeagueLookupIsPaidForOnce pins the other
// tolerate-and-skip site.
//
// canonicalPlacingPerEvent (stats.go) resolves one FetchLeagueInfo per
// distinct league to decide which of a player's placings at an event is
// the flagship one, and skips any league it can't resolve — "an
// unresolvable league just means this row can't win flagship status —
// not a failed request". That is sound behaviour and identical in shape
// to the one that caused v0.19.7: /api/me/events tolerated a dead event
// the same way, and because the response still looked correct, two
// permanently-deleted registrations cost a real BCP round trip on every
// single page load for weeks without anyone noticing.
//
// The fix for that was negative caching in bcp.Cache (goneTTL), which
// covers this call site too — but nothing asserted it here, and an
// untested shared fix is exactly how the first one hid. So: a league
// BCP says does not exist must be asked about once, not once per
// request.
func TestLatencyBudget_ADeadLeagueLookupIsPaidForOnce(t *testing.T) {
	store, cookie := linkedSession(t)

	counts := &countingBCP{watched: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/eventplacings", func(w http.ResponseWriter, _ *http.Request) {
		counts.enter("/eventplacings")
		defer counts.leave("/eventplacings")
		_, _ = w.Write([]byte(`{"data": [
			{"placing": 3, "leagueId": "league-gone", "event": {"id": "evt-1", "name": "Ended Event", "eventDate": "2024-01-01T00:00:00.000Z"}}
		]}`))
	})
	mux.HandleFunc("/players", func(w http.ResponseWriter, _ *http.Request) {
		counts.enter("/players")
		defer counts.leave("/players")
		_, _ = w.Write([]byte(`{"data": []}`))
	})
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, _ *http.Request) {
		counts.enter("/events/:id")
		defer counts.leave("/events/:id")
		_, _ = w.Write([]byte(`{"id": "evt-1", "name": "Ended Event", "status": {"started": true, "ended": true}}`))
	})
	// The league this account's placing is scored under has been
	// deleted. BCP will say so every time it is asked, forever.
	mux.HandleFunc("/leagues/league-gone", func(w http.ResponseWriter, _ *http.Request) {
		counts.enter("/leagues/:id")
		defer counts.leave("/leagues/:id")
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	e := newStatsTestEcho(store, bcp.NewClientWithBaseURL(server.URL))

	const requests = 3
	for range requests {
		getWithSession(t, e, "/api/me/stats", cookie)
	}

	_, _, byPath := counts.snapshot()
	if got := byPath["/leagues/:id"]; got != 1 {
		t.Errorf("a permanently-deleted league was looked up %d times across %d requests, want 1.\n"+
			"By path: %v\n"+
			"A tolerated failure is still a request: this costs a real BCP round trip on every page load "+
			"while the response stays correct, so nothing looks broken. See goneTTL in internal/bcp/cache.go.",
			got, requests, byPath)
	}
}

// eventPageServer serves one *in-progress* event with a league and
// three players — the shape the event page loads on mount.
//
// Live, not concluded, deliberately. A concluded event is the easy
// case: only ended events are written durably, so every lookup is
// protected by both the in-memory cache and Postgres, and a budget
// built on one cannot fail when the other breaks. Mid-event is the
// case CLAUDE.md's two-second rule is actually written about — nothing
// persists, the 60-second in-memory cache is the only thing between
// this page and BCP, and the round counts below are what a phone on
// venue wifi pays.
func eventPageServer(t *testing.T) (*httptest.Server, *countingBCP) {
	t.Helper()
	counts := &countingBCP{watched: map[string]bool{}}

	handle := func(path, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			counts.enter(path)
			defer counts.leave(path)
			time.Sleep(stubDwell)
			_, _ = w.Write([]byte(body))
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", handle("/events/:id", `{"id": "evt-1", "name": "Live Event",
		"status": {"started": true, "ended": false, "currentRound": 3, "numberOfRounds": 5},
		"leagues": [{"id": "league-1", "name": "Flagship"}]}`))
	mux.HandleFunc("/events/evt-1/players", handle("/events/:id/players", `{"active": [
		{"id": "p1", "user": {"id": "u1", "firstName": "Ada", "lastName": "L"}, "faction": {"name": "Necrons"}},
		{"id": "p2", "user": {"id": "u2", "firstName": "Bo", "lastName": "M"}, "faction": {"name": "Orks"}},
		{"id": "p3", "user": {"id": "u3", "firstName": "Cy", "lastName": "N"}, "faction": {"name": "Tau"}}
	]}`))
	mux.HandleFunc("/events/evt-1/teamplayers", handle("/events/:id/teamplayers", `{"active": []}`))
	mux.HandleFunc("/events/evt-1/pairings", handle("/events/:id/pairings", `{"active": [
		{"id": "pair-1", "round": 3, "table": 1, "player1Id": "p1", "player2Id": "p2", "published": true}
	]}`))
	mux.HandleFunc("/leagues/league-1", handle("/leagues/:id", `{"name": "Flagship", "gw_itc": true, "hobby": false}`))
	mux.HandleFunc("/placings", handle("/placings", `{"data": [{"userId": "u1", "ITCPoints": 91.5, "placing": 12}]}`))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, counts
}

// TestLatencyBudget_EventPageStaysWithinItsUpstreamBudget budgets the
// page people actually wait on.
//
// It loads exactly what the frontend loads on mount and asserts two
// things the rest of this file doesn't cover:
//
//   - event info is fetched once, not once per route that needs it.
//     Three of these routes resolve the same event (/api/events/:id,
//     the ITC league lookup, and — until the write path stops doing it
//     — the roster's own durable-persistence check), so this is the
//     single likeliest thing to regress when that code is touched.
//   - the roster is fetched once no matter how many views read it.
func TestLatencyBudget_EventPageStaysWithinItsUpstreamBudget(t *testing.T) {
	server, counts := eventPageServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	// A durable cache is installed even though this event is live and
	// so nothing will be written to it. Both halves matter: every
	// persist-if-ended check in internal/bcp sits inside
	// `if c.durable != nil`, so omitting it would skip those code paths
	// entirely and the "fetched once" assertions below would guard
	// nothing — while an *ended* fixture would let Postgres absorb a
	// broken in-memory cache and hide the same regression the other
	// way. Live event, durable cache present, nothing persisted: the
	// in-memory cache is on its own, which is the point.
	client.SetDurableCache(newCountingDurable())
	e := newBCPTestEcho(client)

	// Deliberately with the repeats a real load makes. The Overview,
	// Roster, Pairings and Placings views each read the roster; the
	// round board, "my pairings" and a team pairing's expanded boards
	// each read the same round; several panels ask for the same
	// player's ITC ranking. FetchRoundPairings' doc comment promises
	// that costs one upstream request "no matter how many of those
	// views ask for it" — this is what holds it to that. A list with no
	// duplicates cannot: it passes with every cache in the client
	// switched off.
	for _, path := range []string{
		"/api/events/evt-1",
		"/api/events/evt-1/players",
		"/api/itc/leagues/event/evt-1",
		"/api/events/evt-1/pairings?type=Pairing&round=3",
		"/api/itc/rankings?leagueId=league-1&userId=u1",
		"/api/itc/rankings?leagueId=league-1&userId=u2",
		"/api/itc/rankings?leagueId=league-1&userId=u3",
		// second wave: the same data, different views
		"/api/events/evt-1/players",
		"/api/events/evt-1/pairings?type=Pairing&round=3",
		"/api/events/evt-1",
		"/api/itc/rankings?leagueId=league-1&userId=u1",
		"/api/events/evt-1/pairings?type=Pairing&round=3",
		"/api/events/evt-1/players",
	} {
		if rec := doBCPRequest(e, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (body: %s)", path, rec.Code, rec.Body.String())
		}
	}

	total, _, byPath := counts.snapshot()
	if total > eventPageUpstreamBudget {
		t.Errorf("one event-page load made %d upstream requests, budget is %d.\nBy path: %v\n"+
			"This is the page used mid-event on venue wifi — see CLAUDE.md's two-second rule.",
			total, eventPageUpstreamBudget, byPath)
	}
	if got := byPath["/events/:id"]; got != 1 {
		t.Errorf("event info fetched %d times across one page load, want 1.\nBy path: %v\n"+
			"Several routes resolve the same event; they are meant to share one cached lookup.", got, byPath)
	}
	if got := byPath["/events/:id/players"]; got != 1 {
		t.Errorf("roster fetched %d times across one page load, want 1 — "+
			"every view that shows a player reads the same roster", got)
	}
	if got := byPath["/events/:id/pairings"]; got != 1 {
		t.Errorf("one round's pairings fetched %d times across one page load, want 1.\n"+
			"The board, \"my pairings\" and a team pairing's individual boards are all "+
			"derived from the same round — see FetchRoundPairings' doc comment.", got)
	}
	if got := byPath["/placings"]; got != 3 {
		t.Errorf("ITC rankings cost %d upstream calls for 3 players, want 3.\nBy path: %v\n"+
			"If this dropped, a batch form was found and the budget above should come down with it; "+
			"if it rose, the per-player cache stopped working.", got, byPath)
	}
}
