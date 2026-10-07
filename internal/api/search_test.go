package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/geocode"
)

type fakePlaces struct {
	got    []string
	places []geocode.Place
	err    error
}

func (f *fakePlaces) Search(_ context.Context, q string) ([]geocode.Place, error) {
	f.got = append(f.got, q)
	return f.places, f.err
}

// recordingBCP serves /events, recording each request's query, and answers
// with n events and a next cursor.
func recordingBCP(t *testing.T, n int) (*httptest.Server, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var seen []url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Query())
		mu.Unlock()
		items := make([]string, n)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id": "e%d", "name": "Open %d", "eventDate": "2026-11-07", "city": "Lindsay", "country": "Canada", "totalPlayers": 24}`, i, i)
		}
		_, _ = fmt.Fprintf(w, `{"data": [%s], "nextKey": "next"}`, strings.Join(items, ","))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values(nil), seen...)
	}
}

func newSearchTestEcho(client *bcp.Client, places placeLookup) *echo.Echo {
	e := echo.New()
	passThrough := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	NewSearchHandler(client, places).Register(e, passThrough, passThrough)
	return e
}

func getRec(e *echo.Echo, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestSearch_RefusesBadFilters(t *testing.T) {
	server, _ := recordingBCP(t, 1)
	e := newSearchTestEcho(bcp.NewClientWithBaseURL(server.URL), &fakePlaces{})
	cases := map[string]string{
		"nothing to search by":    "/api/event-search",
		"name too short":          "/api/event-search?q=ka",
		"name too long":           "/api/event-search?q=" + strings.Repeat("a", 101),
		"latitude out of range":   "/api/event-search?lat=91&lon=0",
		"longitude missing":       "/api/event-search?lat=39.7",
		"radius not offered":      "/api/event-search?lat=39.7&lon=-105&radius=60",
		"date in the wrong shape": "/api/event-search?q=open&from=10/31/2026",
		"end before start":        "/api/event-search?q=open&from=2026-11-01&to=2026-10-01",
		"range over a year":       "/api/event-search?q=open&from=2026-01-01&to=2027-06-01",
		"oversized cursor":        "/api/event-search?q=open&cursor=" + strings.Repeat("c", 2049),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := getRec(e, path); rec.Code != http.StatusBadRequest {
				t.Errorf("status %d, want 400", rec.Code)
			}
		})
	}
}

func TestSearch_PassesFiltersToBCP(t *testing.T) {
	server, seen := recordingBCP(t, 1)
	e := newSearchTestEcho(bcp.NewClientWithBaseURL(server.URL), &fakePlaces{})

	rec := getRec(e, "/api/event-search?q=%20Kawartha%20%20Open&lat=39.7392&lon=-104.9903&radius=100&from=2026-10-10&to=2026-10-12")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	q := seen()[0]
	if got := q.Get("searchString"); got != "kawartha open" {
		t.Errorf("searchString = %q, want lowercase and trimmed", got)
	}
	var loc struct {
		Distance int `json:"distance"`
		Center   struct {
			Lat  float64 `json:"lat"`
			Long float64 `json:"long"`
		} `json:"center"`
	}
	if err := json.Unmarshal([]byte(q.Get("location")), &loc); err != nil {
		t.Fatalf("location %q: %v", q.Get("location"), err)
	}
	if loc.Distance != 100 || loc.Center.Lat != 39.74 || loc.Center.Long != -104.99 {
		t.Errorf("location = %+v, want 100 mi around 39.74,-104.99 (rounded)", loc)
	}
	if q.Get("excludeOnline") != "true" {
		t.Error("a location search must exclude online events, or BCP ignores the location")
	}
	if q.Get("startDate") != "2026-10-10T00:00:00Z" || q.Get("endDate") != "2026-10-12T23:59:59Z" {
		t.Errorf("dates = %s..%s", q.Get("startDate"), q.Get("endDate"))
	}

	var page bcp.EventSearchPage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Results) != 1 || page.Results[0].Location != "Lindsay, Canada" || page.NextCursor != "" {
		t.Errorf("page = %+v (a short page has no next cursor)", page)
	}
}

func TestSearch_LocationAloneAndNameAlone(t *testing.T) {
	server, seen := recordingBCP(t, 1)
	e := newSearchTestEcho(bcp.NewClientWithBaseURL(server.URL), &fakePlaces{})

	if rec := getRec(e, "/api/event-search?lat=39.74&lon=-104.99"); rec.Code != http.StatusOK {
		t.Fatalf("location only: status %d", rec.Code)
	}
	if rec := getRec(e, "/api/event-search?q=open"); rec.Code != http.StatusOK {
		t.Fatalf("name only: status %d", rec.Code)
	}
	got := seen()
	if got[0].Has("searchString") || !strings.Contains(got[0].Get("location"), `"distance":50`) {
		t.Errorf("location only sent %v, want no searchString and the default 50 mi", got[0])
	}
	if got[1].Has("location") || got[1].Has("excludeOnline") {
		t.Errorf("name only sent %v, want no location filter", got[1])
	}
}

func TestSearch_CachesPerFiltersAndPages(t *testing.T) {
	server, seen := recordingBCP(t, 25)
	e := newSearchTestEcho(bcp.NewClientWithBaseURL(server.URL), &fakePlaces{})

	getRec(e, "/api/event-search?q=open")
	getRec(e, "/api/event-search?q=Open")
	getRec(e, "/api/event-search?q=open&from=2026-10-10")
	rec := getRec(e, "/api/event-search?q=open&cursor=next")
	if n := len(seen()); n != 3 {
		t.Errorf("BCP calls = %d, want 3 (the repeat is cached; new dates and a new page aren't)", n)
	}
	if got := seen()[2].Get("nextKey"); got != "next" {
		t.Errorf("nextKey = %q, want the cursor passed through", got)
	}
	var page bcp.EventSearchPage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if page.NextCursor != "next" {
		t.Errorf("a full page's next cursor = %q, want next", page.NextCursor)
	}
}

func TestPlaces_NormalizesAndAnswers(t *testing.T) {
	places := &fakePlaces{places: []geocode.Place{{Name: "Denver, Colorado, United States", Lat: 39.74, Lon: -104.99}}}
	e := newSearchTestEcho(bcp.NewClient(), places)

	if rec := getRec(e, "/api/places?q=d"); rec.Code != http.StatusBadRequest {
		t.Errorf("1-character place: status %d, want 400", rec.Code)
	}
	rec := getRec(e, "/api/places?q=%20Denver%20%20CO")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(places.got) != 1 || places.got[0] != "denver co" {
		t.Errorf("lookup got %q, want the normalized name", places.got)
	}
	var got []geocode.Place
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Lat != 39.74 {
		t.Errorf("places = %+v", got)
	}

	places.err = geocode.ErrUnavailable
	if rec := getRec(e, "/api/places?q=denver"); rec.Code != http.StatusBadGateway {
		t.Errorf("lookup failure: status %d, want 502", rec.Code)
	}
}

func TestSearch_CapacityOnlyForLimitedVisibleSingles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [
			{"id": "singles", "name": "A", "totalPlayers": 28, "numTickets": 40},
			{"id": "team", "name": "B", "teamEvent": true, "totalPlayers": 100, "numTickets": 40},
			{"id": "unlimited", "name": "C", "totalPlayers": 85, "numTickets": 0},
			{"id": "hidden", "name": "D", "totalPlayers": 12, "numTickets": 20, "hidePlayerCount": true}
		]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	e := newSearchTestEcho(bcp.NewClientWithBaseURL(server.URL), &fakePlaces{})

	var page bcp.EventSearchPage
	_ = json.Unmarshal(getRec(e, "/api/event-search?q=open").Body.Bytes(), &page)
	got := map[string][2]string{}
	show := func(p *int) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	for _, r := range page.Results {
		got[r.ID] = [2]string{show(r.PlayerCount), show(r.Capacity)}
	}
	want := map[string][2]string{
		"singles":   {"28", "40"},
		"team":      {"100", "-"},
		"unlimited": {"85", "-"},
		"hidden":    {"-", "-"},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: players/capacity = %v, want %v", id, got[id], w)
		}
	}
}

func TestSearch_DistanceOnlyOnLocationSearches(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"id": "e1", "name": "Open", "coordinate": [-105.0745677, 39.8838055]}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	e := newSearchTestEcho(bcp.NewClientWithBaseURL(server.URL), &fakePlaces{})

	var near, named bcp.EventSearchPage
	_ = json.Unmarshal(getRec(e, "/api/event-search?lat=39.74&lon=-104.99").Body.Bytes(), &near)
	_ = json.Unmarshal(getRec(e, "/api/event-search?q=open").Body.Bytes(), &named)
	if d := near.Results[0].DistanceMiles; d == nil || *d != 11 {
		t.Errorf("location search distance = %v, want 11", d)
	}
	if named.Results[0].DistanceMiles != nil {
		t.Error("a name-only search has no centre, so no distance")
	}
}
