package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/timing"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

type fakeDurableReader struct{ snap timing.Snapshot }

func (f fakeDurableReader) ReadTimings() timing.Snapshot { return f.snap }

func newCostsTestEcho(store userStore, client *bcp.Client, durable durableReader) *echo.Echo {
	e := echo.New()
	NewCostsHandler(store, client, durable).Register(e)
	return e
}

func adminSession(t *testing.T, store *fakeUserStore) *http.Cookie {
	t.Helper()
	cookie, userID := signedInSession(t, store)
	if err := store.SetRole(context.Background(), userID, user.RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	return cookie
}

// TestCosts_RequiresAdmin — this is operational detail about the
// deployment. A signed-in ordinary account has no business with it.
func TestCosts_RequiresAdmin(t *testing.T) {
	store := newFakeUserStore()
	cookie, _ := signedInSession(t, store)
	e := newCostsTestEcho(store, bcp.NewClient(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/costs", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Error("a non-admin account read the costs report")
	}
}

// TestCosts_ReportsAssumedAgainstObserved is the point of the endpoint:
// the constant and the reality, side by side, so drift is visible
// rather than discovered months later.
func TestCosts_ReportsAssumedAgainstObserved(t *testing.T) {
	store := newFakeUserStore()
	cookie := adminSession(t, store)

	durable := fakeDurableReader{snap: timing.Snapshot{
		Count: 12, P50: 83 * time.Millisecond, P90: 140 * time.Millisecond,
	}}
	e := newCostsTestEcho(store, bcp.NewClient(), durable)

	rec := getWithSession(t, e, "/api/admin/costs", cookie)

	var resp costsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if resp.DurableRead.AssumedMs != bcp.DurableReadCost.Milliseconds() {
		t.Errorf("assumed durable read = %dms, want the constant's %dms",
			resp.DurableRead.AssumedMs, bcp.DurableReadCost.Milliseconds())
	}
	if resp.DurableRead.ObservedP50Ms == nil || *resp.DurableRead.ObservedP50Ms != 83 {
		t.Errorf("observed durable p50 = %v, want 83", resp.DurableRead.ObservedP50Ms)
	}
	if resp.DurableRead.SampleCount != 12 {
		t.Errorf("sample count = %d, want 12 — a percentile without its count reads as authoritative "+
			"whether it came from twelve samples or two", resp.DurableRead.SampleCount)
	}
}

// TestCosts_UnsampledCostsOmitTheObservation — reporting 0ms for
// something nothing has measured would read as "instant", which is the
// opposite of "unknown".
func TestCosts_UnsampledCostsOmitTheObservation(t *testing.T) {
	store := newFakeUserStore()
	cookie := adminSession(t, store)
	e := newCostsTestEcho(store, bcp.NewClient(), nil)

	rec := getWithSession(t, e, "/api/admin/costs", cookie)

	var top map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	raw := map[string]map[string]any{}
	for _, key := range []string{"bcpFetch", "durableRead", "ownOverhead"} {
		section, ok := top[key].(map[string]any)
		if !ok {
			t.Fatalf("%s missing from the response", key)
		}
		raw[key] = section
	}

	for _, key := range []string{"bcpFetch", "durableRead", "ownOverhead"} {
		if _, present := raw[key]["observedP50Ms"]; present {
			t.Errorf("%s reported an observed p50 with nothing sampled; absent is the honest answer", key)
		}
		if raw[key]["assumedMs"] == nil {
			t.Errorf("%s did not report what the model assumes", key)
		}
	}

	// The one that can never be observed from in here has to say so,
	// or its permanently-absent reading looks like a bug.
	if note, _ := raw["ownOverhead"]["note"].(string); note == "" {
		t.Error("ownOverhead carries no note explaining why it is never observed from inside the process")
	}
}
