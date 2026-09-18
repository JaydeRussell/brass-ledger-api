package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

func newDossierTestEcho(store userStore, client *bcp.Client) *echo.Echo {
	e := echo.New()
	NewDossierHandler(store, client).Register(e, noopRateLimit)
	return e
}

// noopRateLimit stands in for the real per-IP limiter (built in main.go,
// not exercised here) so DossierHandler.Register's signature can be
// satisfied without a real rate-limiter store spinning up per test.
func noopRateLimit(next echo.HandlerFunc) echo.HandlerFunc {
	return next
}

func TestDossier_UnknownBcpUserID(t *testing.T) {
	e := newDossierTestEcho(newFakeUserStore(), bcp.NewClient())
	req := httptest.NewRequest(http.MethodGet, "/api/players/nobody/dossier", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDossier_UnapprovedAccountNotFound(t *testing.T) {
	store := newFakeUserStore()
	store.newUserStatus = user.StatusPending
	_, userID := signedInSession(t, store)
	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	e := newDossierTestEcho(store, bcp.NewClient())
	req := httptest.NewRequest(http.MethodGet, "/api/players/bcp-user-1/dossier", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// A pending account's dossier should read exactly like a nonexistent
	// one (see Dossier's doc comment) — not a distinct "not approved yet"
	// error that would confirm the account exists.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDossier_OptedOutNotFound(t *testing.T) {
	store := newFakeUserStore()
	_, userID := signedInSession(t, store)
	ctx := context.Background()
	if err := store.SetBcpUserID(ctx, userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}
	if err := store.SetDossierPublic(ctx, userID, false); err != nil {
		t.Fatalf("SetDossierPublic: %v", err)
	}

	e := newDossierTestEcho(store, bcp.NewClient())
	req := httptest.NewRequest(http.MethodGet, "/api/players/bcp-user-1/dossier", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDossier_PublicApprovedAccount(t *testing.T) {
	store := newFakeUserStore()
	_, userID := signedInSession(t, store)
	ctx := context.Background()
	if err := store.SetBcpUserID(ctx, userID, "bcp-user-1"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}
	// Confirm the account's own display name (set via signedInSession)
	// round-trips into the dossier response.
	acct, err := store.GetUserByBcpUserID(ctx, "bcp-user-1")
	if err != nil {
		t.Fatalf("GetUserByBcpUserID: %v", err)
	}

	server := stubBCPStatsServer(t)
	client := bcp.NewClientWithBaseURL(server.URL)
	e := newDossierTestEcho(store, client)

	req := httptest.NewRequest(http.MethodGet, "/api/players/bcp-user-1/dossier", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp dossierResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("couldn't parse body: %v", err)
	}
	if resp.Name != acct.Name {
		t.Errorf("name = %q, want %q", resp.Name, acct.Name)
	}
	if !resp.Linked || resp.TotalEvents != 5 {
		t.Errorf("expected the same aggregated stats stubBCPStatsServer produces for /api/me/stats, got linked=%v totalEvents=%d", resp.Linked, resp.TotalEvents)
	}
}

func TestSetDossierVisibility_RequiresSignIn(t *testing.T) {
	e := newMeTestEcho(newFakeUserStore(), bcp.NewClient())
	req := httptest.NewRequest(http.MethodPost, "/api/me/dossier-visibility", strings.NewReader(`{"public": false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSetDossierVisibility_TurnsOff(t *testing.T) {
	store := newFakeUserStore()
	cookie, userID := signedInSession(t, store)
	e := newMeTestEcho(store, bcp.NewClient())

	req := httptest.NewRequest(http.MethodPost, "/api/me/dossier-visibility", strings.NewReader(`{"public": false}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	if err := store.SetBcpUserID(context.Background(), userID, "bcp-user-2"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}
	dossierEcho := newDossierTestEcho(store, bcp.NewClient())
	dReq := httptest.NewRequest(http.MethodGet, "/api/players/bcp-user-2/dossier", nil)
	dRec := httptest.NewRecorder()
	dossierEcho.ServeHTTP(dRec, dReq)
	if dRec.Code != http.StatusNotFound {
		t.Errorf("dossier status after opting out = %d, want %d", dRec.Code, http.StatusNotFound)
	}
}
