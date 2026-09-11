package bcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

// --- shared test helpers -------------------------------------------------
//
// Used across this package's other _test.go files (events_test.go,
// players_test.go, pairings_test.go, placings_test.go, itc_test.go,
// history_test.go) — kept here since none of those files is more
// entitled to "own" them than the others.

// newTestClient builds a Client whose v1/v2/site base URLs all point at
// server — letting these tests exercise the real fetch/parse logic
// without ever reaching the actual BCP API. Thin wrapper over the
// exported NewClientWithBaseURL, which internal/api's tests also use.
func newTestClient(server *httptest.Server) *Client {
	return NewClientWithBaseURL(server.URL)
}

// jsonHandler replies with a fixed status and JSON body regardless of the
// request, which is all these table-driven cases need — each one only
// cares about how the Client parses a given upstream response.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func eventInfoEqual(a, b EventInfo) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func itcRankingEqual(a, b *ItcRanking) bool {
	if a == nil || b == nil {
		return a == b
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
