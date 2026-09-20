package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// stubBCPServer serves just enough of BCP's real response shapes for
// every route BCPHandler wires up to succeed against it — one
// shared stub, reused across the success-path table below, since each
// case only cares about how this service's own route maps a request to
// a response, not about varying the upstream data.
func stubBCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-1", "name": "Test Cup", "leagues": [{"id": "league-1", "name": "Test League"}]}`))
	})
	mux.HandleFunc("/events/evt-1/players", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("placings") == "true" {
			_, _ = w.Write([]byte(`{"active": [{"id": "p1", "user": {"firstName": "A", "lastName": "B"}, "placing": 1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"active": [{"id": "p1", "user": {"firstName": "A", "lastName": "B"}, "faction": {"name": "Necrons"}, "listId": "l1"}]}`))
	})
	mux.HandleFunc("/events/evt-1/teamplayers", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // singles event: no team concept
	})
	mux.HandleFunc("/events/evt-1/pairings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"active": [{"id": "pair-1", "pairingType": "Pairing", "round": 1}]}`))
	})
	mux.HandleFunc("/leagues/league-1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Test League", "gw_itc": true, "hobby": false}`))
	})
	mux.HandleFunc("/placings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"userId": "u1", "ITCPoints": 100}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// alwaysFailServer stands in for BCP being down/erroring, for the
// upstream-failure table below.
func alwaysFailServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server
}

// newBCPTestEcho/doBCPRequest are named distinctly from auth_test.go's
// own newTestEcho/doRequest (same package, so a same-named pair here
// would be a redeclaration — these two files were written independently
// and happened to pick the same generic names) — matching the
// newMeTestEcho/newSyncTestEcho naming convention the other test files
// in this package already use for their own per-feature Echo helpers.
func newBCPTestEcho(client *bcp.Client) *echo.Echo {
	e := echo.New()
	// A pass-through middleware for both params — these tests are about
	// the BCP proxy behavior itself, not the gates in front of it
	// (that's TestBCPHandler_RequiresApproved and
	// TestBCPHandler_Players_OnlyRequiresSession below).
	passThrough := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	NewBCPHandler(client).Register(e, passThrough, passThrough)
	return e
}

func doBCPRequest(e *echo.Echo, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// TestBCPHandler_RequiresApproved checks that these routes are actually
// gated behind sign-in *and* approval now (unlike newBCPTestEcho's
// other callers, which deliberately bypass the gate to test proxy
// behavior on its own) — the whole point of wiring RequireApproved into
// cmd/server/main.go in the first place.
func TestBCPHandler_RequiresApproved(t *testing.T) {
	server := stubBCPServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	store := newFakeUserStore()
	e := echo.New()
	NewBCPHandler(client).Register(e, RequireApproved(store), RequireSession(store))

	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	badCookie := []*http.Cookie{{Name: sessionCookieName, Value: "not-a-real-token"}}
	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1", badCookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid cookie: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	pendingCookie, _ := newSignedInUser(t, store, "pending", user.RoleUser, user.StatusPending)
	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1", []*http.Cookie{pendingCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("pending session: status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	rejectedCookie, _ := newSignedInUser(t, store, "rejected", user.RoleUser, user.StatusRejected)
	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1", []*http.Cookie{rejectedCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("rejected session: status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	approvedCookie, _ := newSignedInUser(t, store, "approved", user.RoleUser, user.StatusApproved)
	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1", []*http.Cookie{approvedCookie}); rec.Code != http.StatusOK {
		t.Errorf("approved session: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestBCPHandler_Players_OnlyRequiresSession checks the one deliberate
// exception to TestBCPHandler_RequiresApproved above: Players is
// reachable by a merely-signed-in (not yet approved) account, since
// that's what the frontend's BCP-profile-linking roster picker needs
// during onboarding — see Register's doc comment for the full
// rationale. Every other route stays behind full approval regardless.
func TestBCPHandler_Players_OnlyRequiresSession(t *testing.T) {
	server := stubBCPServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	store := newFakeUserStore()
	e := echo.New()
	NewBCPHandler(client).Register(e, RequireApproved(store), RequireSession(store))

	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1/players", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	pendingCookie, _ := newSignedInUser(t, store, "players-pending", user.RoleUser, user.StatusPending)
	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1/players", []*http.Cookie{pendingCookie}); rec.Code != http.StatusOK {
		t.Errorf("pending session on Players: status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	// The exception is scoped to Players specifically — the same
	// pending session still can't reach a route that requires approval.
	if rec := doRequest(e, http.MethodGet, "/api/events/evt-1", []*http.Cookie{pendingCookie}); rec.Code != http.StatusForbidden {
		t.Errorf("pending session on EventInfo: status = %d, want %d, body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestBCPHandler_Success checks every route's happy path: status
// 200, and a shape/field that proves the response is this route's own
// data rather than another route's or an empty default value.
func TestBCPHandler_Success(t *testing.T) {
	server := stubBCPServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newBCPTestEcho(client)

	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string // substring expected somewhere in the JSON response
	}{
		{"event info", "/api/events/evt-1", http.StatusOK, `"name":"Test Cup"`},
		{"players", "/api/events/evt-1/players", http.StatusOK, `"name":"A B"`},
		{"pairings", "/api/events/evt-1/pairings?type=Pairing&round=1", http.StatusOK, `"id":"pair-1"`},
		{"placings (individual)", "/api/events/evt-1/placings?team=false", http.StatusOK, `"name":"A B"`},
		{"itc league lookup, found", "/api/itc/leagues/event/evt-1", http.StatusOK, `"leagueId":"league-1"`},
		{"itc ranking, found", "/api/itc/rankings?leagueId=league-1&userId=u1", http.StatusOK, `"points":100`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doBCPRequest(e, http.MethodGet, tc.path)
			if rec.Code != tc.wantStatus {
				t.Fatalf("GET %s: status = %d, want %d (body: %s)", tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("GET %s: body = %s, want it to contain %q", tc.path, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestBCPHandler_Refresh checks that ?refresh=true on Pairings and
// Placings reaches Client.InvalidateRoundPairings/InvalidatePlacings —
// same convention as GET /api/me/events?refresh=true (see me_test.go).
// It only proves the wiring, not the manual-invalidate floor's own
// timing (an immediate ?refresh=true right after the first fetch is
// itself throttled, same as every other Invalidate* test in this
// codebase — see internal/bcp/cache_test.go's TestCache_Invalidate for
// where the floor's actual behavior is covered).
func TestBCPHandler_Refresh(t *testing.T) {
	var pairingsCalls, placingsCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1/pairings", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pairingsCalls, 1)
		_, _ = w.Write([]byte(`{"active": []}`))
	})
	mux.HandleFunc("/events/evt-1/players", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&placingsCalls, 1)
		_, _ = w.Write([]byte(`{"active": []}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newBCPTestEcho(client)

	// Pairings: first request populates the cache; ?refresh=true right
	// after is throttled by the manual-invalidate floor, so it's still
	// only one real upstream call.
	doBCPRequest(e, http.MethodGet, "/api/events/evt-1/pairings?type=Pairing&round=1")
	doBCPRequest(e, http.MethodGet, "/api/events/evt-1/pairings?type=Pairing&round=1&refresh=true")
	if pairingsCalls != 1 {
		t.Errorf("pairings upstream calls = %d, want 1 (immediate refresh should be throttled)", pairingsCalls)
	}

	// Same for placings.
	doBCPRequest(e, http.MethodGet, "/api/events/evt-1/placings?team=false")
	doBCPRequest(e, http.MethodGet, "/api/events/evt-1/placings?team=false&refresh=true")
	if placingsCalls != 1 {
		t.Errorf("placings upstream calls = %d, want 1 (immediate refresh should be throttled)", placingsCalls)
	}
}

// TestBCPHandler_ParamValidation checks that malformed query
// params are rejected with 400 before this service ever calls out to
// BCP — the client here is deliberately pointed at a server that would
// fail any real request, so a case that reaches it (a bug in the
// validation) would show up as a 502 instead of the expected 400.
func TestBCPHandler_ParamValidation(t *testing.T) {
	server := alwaysFailServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newBCPTestEcho(client)

	cases := []struct {
		name string
		path string
	}{
		{"pairings: missing type", "/api/events/evt-1/pairings?round=1"},
		{"pairings: invalid type", "/api/events/evt-1/pairings?type=NotAType&round=1"},
		{"pairings: missing round", "/api/events/evt-1/pairings?type=Pairing"},
		{"pairings: non-numeric round", "/api/events/evt-1/pairings?type=Pairing&round=abc"},
		{"pairings: round zero", "/api/events/evt-1/pairings?type=Pairing&round=0"},
		{"pairings: negative round", "/api/events/evt-1/pairings?type=Pairing&round=-1"},
		{"itc rankings: missing leagueId", "/api/itc/rankings?userId=u1"},
		{"itc rankings: missing userId", "/api/itc/rankings?leagueId=league-1"},
		{"itc rankings: missing both", "/api/itc/rankings"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doBCPRequest(e, http.MethodGet, tc.path)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s: status = %d, want %d (body: %s)", tc.path, rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("GET %s: response body isn't the expected {error} shape: %v", tc.path, err)
			}
			if body["error"] == "" {
				t.Errorf("GET %s: expected a non-empty \"error\" message", tc.path)
			}
		})
	}
}

// TestBCPHandler_UpstreamFailure checks that every route maps a
// BCP failure to 502 (this service is a working proxy whose dependency
// failed), not 500 (which would suggest a bug here) or a silent success.
func TestBCPHandler_UpstreamFailure(t *testing.T) {
	server := alwaysFailServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newBCPTestEcho(client)

	cases := []struct {
		name string
		path string
	}{
		{"event info", "/api/events/evt-1"},
		{"players", "/api/events/evt-1/players"},
		{"pairings", "/api/events/evt-1/pairings?type=Pairing&round=1"},
		{"placings", "/api/events/evt-1/placings"},
		{"itc league lookup", "/api/itc/leagues/event/evt-1"},
		{"itc ranking", "/api/itc/rankings?leagueId=league-1&userId=u1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doBCPRequest(e, http.MethodGet, tc.path)
			if rec.Code != http.StatusBadGateway {
				t.Errorf("GET %s: status = %d, want %d (body: %s)", tc.path, rec.Code, http.StatusBadGateway, rec.Body.String())
			}
		})
	}
}

// TestBCPHandler_ItcLeagueNotFound checks the one success path
// that isn't a passthrough of BCP's data: no matching league resolves to
// a 200 with a null leagueId, not a 404 or an error — "not found" is a
// normal, expected outcome here (e.g. a game system with no ITC league).
func TestBCPHandler_ItcLeagueNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "evt-1", "name": "Local RTT", "leagues": [{"id": "local-league", "name": "Local Store League"}]}`))
	})
	mux.HandleFunc("/leagues/local-league", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name": "Local Store League", "gw_itc": false, "hobby": false}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newBCPTestEcho(client)

	rec := doBCPRequest(e, http.MethodGet, "/api/itc/leagues/event/evt-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"leagueId":null`) {
		t.Errorf("body = %s, want it to contain %q", rec.Body.String(), `"leagueId":null`)
	}
}

// TestBCPHandler_ItcRankingNotFound mirrors the case above for
// rankings: no ranking for this player in this league is a normal 200
// with a null body, not an error.
func TestBCPHandler_ItcRankingNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/placings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newBCPTestEcho(client)

	rec := doBCPRequest(e, http.MethodGet, "/api/itc/rankings?leagueId=league-1&userId=u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "null" {
		t.Errorf("body = %q, want the literal JSON null", rec.Body.String())
	}
}

// TestBcpError checks the shared error-mapping helper directly: any
// upstream error becomes a 502 with its message under an "error" key.
func TestBcpError(t *testing.T) {
	cases := []struct {
		name    string
		errText string
	}{
		{"simple message", "boom"},
		{"message with quotes", `upstream said "no"`},
		{"empty message", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			if err := bcpError(c, errorString(tc.errText)); err != nil {
				t.Fatalf("bcpError returned an error itself: %v", err)
			}
			if rec.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response body isn't the expected {error} shape: %v", err)
			}
			if body["error"] != tc.errText {
				t.Errorf(`body["error"] = %q, want %q`, body["error"], tc.errText)
			}
		})
	}
}

// errorString is a trivial error whose message is exactly the string it
// wraps — used above to control bcpError's input precisely, including
// the empty-message edge case a real error type wouldn't normally have.
type errorString string

func (e errorString) Error() string { return string(e) }

// roundsStub serves each round's pairings and records the concurrency
// it saw, so a test can tell a parallel fan-out from a serial one.
func roundsStub(t *testing.T) (*httptest.Server, *countingBCP) {
	t.Helper()
	counts := &countingBCP{watched: map[string]bool{"/events/:id/pairings": true}}
	mux := http.NewServeMux()
	mux.HandleFunc("/events/evt-1/pairings", func(w http.ResponseWriter, r *http.Request) {
		counts.enter("/events/:id/pairings")
		defer counts.leave("/events/:id/pairings")
		time.Sleep(stubDwell)
		round := r.URL.Query().Get("round")
		// Deliberately no "round" field: BCP's own is nullable, and the
		// handler is meant to stamp what it asked for.
		_, _ = fmt.Fprintf(w, `{"active": [{"id": "pair-r%s", "table": 1}]}`, round)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, counts
}

// TestPairings_MultipleRoundsInOneRequest covers the batched form.
//
// "My pairings" and the placings round strip each walked rounds in a
// serial `await` loop in the browser, so a five-round event meant five
// sequential round trips before anything rendered. Same number of BCP
// requests either way — these are the same rounds, fetched through the
// same cache — but one HTTP request instead of five, and overlapped
// instead of chained.
func TestPairings_MultipleRoundsInOneRequest(t *testing.T) {
	server, counts := roundsStub(t)
	e := newBCPTestEcho(bcp.NewClientWithBaseURL(server.URL))

	rec := doBCPRequest(e, http.MethodGet, "/api/events/evt-1/pairings?type=Pairing&rounds=1,2,3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var got []bcp.PairingRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records across three rounds, want 3", len(got))
	}

	// Each record has to say which round it belongs to, or a caller
	// that asked for several at once cannot tell them apart.
	for i, want := range []int{1, 2, 3} {
		if got[i].Round == nil {
			t.Fatalf("record %d has no round; BCP's own field is nullable, so the handler must stamp "+
				"the round it asked for", i)
		}
		if *got[i].Round != want {
			t.Errorf("record %d is round %d, want %d — results must come back in the order asked for",
				i, *got[i].Round, want)
		}
	}

	if peak := counts.watchedPeak(); peak < 2 {
		t.Errorf("peak concurrent round fetches was %d, want at least 2 — "+
			"fetching them one after another here just moves the serial chain from the browser to the server",
			peak)
	}
	if peak := counts.watchedPeak(); peak > maxConcurrentRounds {
		t.Errorf("peak concurrent round fetches was %d, above the %d bound — "+
			"BCP should not see one page load as a burst", peak, maxConcurrentRounds)
	}
}

func TestPairings_RoundParamValidation(t *testing.T) {
	server, counts := roundsStub(t)
	e := newBCPTestEcho(bcp.NewClientWithBaseURL(server.URL))

	t.Run("the single-round form still works", func(t *testing.T) {
		rec := doBCPRequest(e, http.MethodGet, "/api/events/evt-1/pairings?type=Pairing&round=2")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("a repeated round is fetched once", func(t *testing.T) {
		before, _, _ := counts.snapshot()
		rec := doBCPRequest(e, http.MethodGet, "/api/events/evt-1/pairings?type=Pairing&rounds=7,7,7")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		after, _, _ := counts.snapshot()
		if after-before != 1 {
			t.Errorf("rounds=7,7,7 cost %d upstream requests, want 1", after-before)
		}

		// The upstream count alone does not prove de-duplication: the
		// cache's own in-flight sharing collapses three concurrent
		// fetches of one key into a single request regardless. What a
		// duplicate round actually costs is a duplicated *response* —
		// the same pairings returned three times, which a caller
		// grouping by round would count three times.
		var got []bcp.PairingRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("rounds=7,7,7 returned %d records, want 1 — a repeated round must not "+
				"appear repeatedly in the response", len(got))
		}
	})

	for _, bad := range []string{
		"/api/events/evt-1/pairings?type=Pairing",
		"/api/events/evt-1/pairings?type=Pairing&rounds=0",
		"/api/events/evt-1/pairings?type=Pairing&rounds=1,nope",
		"/api/events/evt-1/pairings?type=Pairing&rounds=-3",
	} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if rec := doBCPRequest(e, http.MethodGet, bad); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}

	t.Run("caps the fan-out", func(t *testing.T) {
		many := make([]string, maxRoundsPerRequest+1)
		for i := range many {
			many[i] = strconv.Itoa(i + 1)
		}
		rec := doBCPRequest(e, http.MethodGet,
			"/api/events/evt-1/pairings?type=Pairing&rounds="+strings.Join(many, ","))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 — a crafted URL must not turn one request "+
				"into an unbounded crawl of BCP", rec.Code)
		}
	})
}
