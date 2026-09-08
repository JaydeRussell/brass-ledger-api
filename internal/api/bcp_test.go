package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
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
		_, _ = w.Write([]byte(`{"id": "evt-1", "name": "Test Cup"}`))
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
	mux.HandleFunc("/leagues", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"id": "league-1", "gw_itc": true, "hobby": false}]}`))
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
	NewBCPHandler(client).Register(e)
	return e
}

func doBCPRequest(e *echo.Echo, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
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
		{"itc league lookup, found", "/api/itc/leagues/gs-1", http.StatusOK, `"leagueId":"league-1"`},
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
		{"itc league lookup", "/api/itc/leagues/gs-1"},
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
	mux.HandleFunc("/leagues", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": []}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newBCPTestEcho(client)

	rec := doBCPRequest(e, http.MethodGet, "/api/itc/leagues/gs-1")
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
