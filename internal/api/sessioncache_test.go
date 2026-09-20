package api

import (
	"context"
	"errors"
	"testing"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// countingUserStore records how often the session lookup actually
// reaches the store underneath — which, in production, is a query
// against Neon.
type countingUserStore struct {
	*fakeUserStore
	lookups int
}

func (c *countingUserStore) GetUserBySession(ctx context.Context, token string) (user.User, error) {
	c.lookups++
	return c.fakeUserStore.GetUserBySession(ctx, token)
}

func newCountingStore(t *testing.T) (*countingUserStore, *CachedUserStore, string, int64) {
	t.Helper()
	fake := newFakeUserStore()
	counting := &countingUserStore{fakeUserStore: fake}
	cookie, userID := signedInSession(t, fake)
	return counting, NewCachedUserStore(counting), cookie.Value, userID
}

// TestSessionCache_RepeatedLookupsCostOneQuery is the whole point: a
// page load fires several requests at once and every one of them
// resolves the same session before doing anything else.
func TestSessionCache_RepeatedLookupsCostOneQuery(t *testing.T) {
	counting, cached, token, _ := newCountingStore(t)

	for i := range 6 {
		if _, err := cached.GetUserBySession(context.Background(), token); err != nil {
			t.Fatalf("lookup %d: %v", i+1, err)
		}
	}

	if counting.lookups != 1 {
		t.Errorf("six session lookups cost %d queries, want 1 — this is one Neon round trip "+
			"per request before any handler does work", counting.lookups)
	}
}

// TestSessionCache_SignOutTakesEffectImmediately is the property that
// justifies the cache existing.
//
// Opaque server-side tokens were chosen over JWTs specifically so a
// session can be revoked on sign-out rather than staying valid until
// its own expiry. A cache that outlived DeleteSession would hand that
// back, with a grace period an attacker gets to use.
func TestSessionCache_SignOutTakesEffectImmediately(t *testing.T) {
	_, cached, token, _ := newCountingStore(t)

	if _, err := cached.GetUserBySession(context.Background(), token); err != nil {
		t.Fatalf("pre-logout lookup: %v", err)
	}
	if err := cached.DeleteSession(context.Background(), token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	if _, err := cached.GetUserBySession(context.Background(), token); err == nil {
		t.Error("a deleted session still resolved — sign-out has to revoke immediately, " +
			"not at the end of a cache TTL")
	}
}

// TestSessionCache_ApprovalIsVisibleImmediately covers the staleness
// that bites a real person rather than a threat model.
//
// The cached value is a whole user.User, role and status included. An
// admin approving a pending account changes that row, and the account
// holder is very likely refreshing the page at that exact moment.
func TestSessionCache_ApprovalIsVisibleImmediately(t *testing.T) {
	counting, cached, token, userID := newCountingStore(t)

	// Seeded through the inner store on purpose: going via cached would
	// flush the cache as part of the setup and the test would pass
	// without proving anything.
	if err := counting.fakeUserStore.SetStatus(context.Background(), userID, user.StatusPending); err != nil {
		t.Fatalf("seeding a pending account: %v", err)
	}

	before, err := cached.GetUserBySession(context.Background(), token)
	if err != nil {
		t.Fatalf("pre-approval lookup: %v", err)
	}
	if before.Status != user.StatusPending {
		t.Fatalf("fixture resolved status %q, want it pending before approval", before.Status)
	}

	if err := cached.SetStatus(context.Background(), userID, user.StatusApproved); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	after, err := cached.GetUserBySession(context.Background(), token)
	if err != nil {
		t.Fatalf("post-approval lookup: %v", err)
	}
	if after.Status != user.StatusApproved {
		t.Errorf("after approval the session still resolved to status %q, want %q — "+
			"a write to a users row has to drop what the cache is holding",
			after.Status, user.StatusApproved)
	}
}

// TestSessionCache_LinkingABcpProfileIsVisibleImmediately is the same
// hazard on the path a user walks themselves: /welcome links a profile
// and then goes straight to a page that reads BcpUserID off the session.
func TestSessionCache_LinkingABcpProfileIsVisibleImmediately(t *testing.T) {
	_, cached, token, userID := newCountingStore(t)

	if _, err := cached.GetUserBySession(context.Background(), token); err != nil {
		t.Fatalf("pre-link lookup: %v", err)
	}
	if err := cached.SetBcpUserID(context.Background(), userID, "bcp-123"); err != nil {
		t.Fatalf("SetBcpUserID: %v", err)
	}

	after, err := cached.GetUserBySession(context.Background(), token)
	if err != nil {
		t.Fatalf("post-link lookup: %v", err)
	}
	if after.BcpUserID != "bcp-123" {
		t.Errorf("after linking, the session still resolved BcpUserID %q, want %q — "+
			"the user would be told they hadn't linked a profile they just linked",
			after.BcpUserID, "bcp-123")
	}
}

// TestSessionCache_FailedLookupsAreNotCached — "not signed in" is the
// answer an attacker retries freely and the answer a just-signed-in
// user needs to stop getting. Remembering it helps neither.
func TestSessionCache_FailedLookupsAreNotCached(t *testing.T) {
	counting, cached, _, _ := newCountingStore(t)

	for range 3 {
		if _, err := cached.GetUserBySession(context.Background(), "not-a-real-token"); err == nil {
			t.Fatal("an unknown token resolved to a user")
		} else if !errors.Is(err, user.ErrSessionNotFound) {
			t.Fatalf("got %v, want ErrSessionNotFound", err)
		}
	}

	if counting.lookups != 3 {
		t.Errorf("three unknown-token lookups cost %d queries, want 3 — a failure must not be cached", counting.lookups)
	}
}
