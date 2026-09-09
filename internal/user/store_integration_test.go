//go:build integration

// These tests exercise *Store against a real Postgres — unlike the rest
// of this codebase's tests (see internal/api's fakeUserStore), there's
// no meaningful way to fake a database transaction/constraint/upsert
// behavior, so this is the one place that actually needs one. Gated
// behind the "integration" build tag so plain `go test ./...` (what
// every other package's tests run under, and what a contributor without
// a local Postgres running would otherwise hit) never requires
// DATABASE_URL to be set. Run explicitly with:
//
//	go test -tags=integration ./internal/user/...
//
// against a real Postgres (see .github/workflows/ci.yml's "integration"
// job, or `make test-integration` against the local docker-compose
// Postgres).
package user

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/db"
)

// uniqueID returns an identifier unique to this specific test run, not
// just this test function — t.Name() alone repeats identically every
// time the suite runs, which is fine against CI's fresh-per-run
// Postgres service container but collides on a hard unique constraint
// (see TestStore_GetUserBySession_ExpiredSession's plain INSERT) the
// second time it's run against a persistent local docker-compose
// Postgres that was never wiped in between.
func uniqueID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

// newTestStore connects to DATABASE_URL, runs every migration, and
// returns a Store backed by it. Every test below scopes its own rows by
// a uniqueID() rather than truncating tables or wrapping in a
// rolled-back transaction — simpler, and Store's methods take a
// *pgxpool.Pool directly rather than something transaction-shaped, so
// per-test transactions aren't a natural fit here anyway. Safe for
// tests to run in parallel against the same running Postgres as a
// result, and safe to re-run repeatedly against a persistent database
// without ever colliding with a previous run's rows.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL must be set to run integration tests — see this file's build-tag comment")
	}

	ctx := context.Background()
	pool, err := db.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	return NewStore(pool)
}

func TestStore_UpsertUserFromGoogle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	googleSub := "google-sub-" + uniqueID(t)

	u, err := store.UpsertUserFromGoogle(ctx, googleSub, "a@example.com", "Alice", "https://example.com/a.png")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle (create): %v", err)
	}
	if u.Email != "a@example.com" || u.Name != "Alice" || u.BcpUserID != "" {
		t.Fatalf("unexpected user on create: %+v", u)
	}
	// Migration 0007's actual DEFAULT clauses, not a Go-level default —
	// this is exactly the kind of thing only a real database can verify
	// meaningfully (see internal/api's fakeUserStore, which deliberately
	// defaults its own fake users to already-approved instead, for
	// unrelated reasons — see its comment).
	if u.Role != RoleUser {
		t.Errorf("new user role = %q, want %q", u.Role, RoleUser)
	}
	if u.Status != StatusPending {
		t.Errorf("new user status = %q, want %q", u.Status, StatusPending)
	}

	// Signing in again with the same googleSub but a changed profile
	// (Google's own name/avatar can change) should update the existing
	// row in place, not create a second one.
	u2, err := store.UpsertUserFromGoogle(ctx, googleSub, "a-new@example.com", "Alice Renamed", "https://example.com/a2.png")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle (update): %v", err)
	}
	if u2.ID != u.ID {
		t.Fatalf("expected the same user id on re-upsert, got %d then %d", u.ID, u2.ID)
	}
	if u2.Email != "a-new@example.com" || u2.Name != "Alice Renamed" {
		t.Fatalf("re-upsert did not update profile fields: %+v", u2)
	}

	// Approving an account, then that account signing in again (a
	// routine profile refresh), must never silently reset it back to
	// pending — see UpsertUserFromGoogle's doc comment.
	if err := store.SetStatus(ctx, u.ID, StatusApproved); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	u3, err := store.UpsertUserFromGoogle(ctx, googleSub, "a-new@example.com", "Alice Renamed Again", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle (re-upsert after approval): %v", err)
	}
	if u3.Status != StatusApproved {
		t.Fatalf("status after a routine re-upsert = %q, want %q (approval must survive a profile refresh)", u3.Status, StatusApproved)
	}
}

func TestStore_SessionLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := uniqueID(t)
	u, err := store.UpsertUserFromGoogle(ctx, "google-sub-"+runID, "b@example.com", "Bob", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}

	token, err := store.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if token == "" {
		t.Fatal("CreateSession returned an empty token")
	}

	got, err := store.GetUserBySession(ctx, token)
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("GetUserBySession returned user %d, want %d", got.ID, u.ID)
	}

	if err := store.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := store.GetUserBySession(ctx, token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetUserBySession after delete: got %v, want ErrSessionNotFound", err)
	}

	// Deleting a session that's already gone (or never existed) isn't an
	// error — see DeleteSession's doc comment.
	if err := store.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession (already gone): %v", err)
	}
	if err := store.DeleteSession(ctx, "never-existed-"+runID); err != nil {
		t.Fatalf("DeleteSession (never existed): %v", err)
	}
}

func TestStore_GetUserBySession_ExpiredSession(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := uniqueID(t)
	u, err := store.UpsertUserFromGoogle(ctx, "google-sub-"+runID, "c@example.com", "Carol", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}

	// CreateSession always sets a future expiry — inserting an
	// already-expired row directly is the only way to exercise
	// GetUserBySession's "expired" branch without waiting SessionDuration.
	token := "expired-" + runID
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3)`,
		token, u.ID, time.Now().Add(-time.Hour),
	); err != nil {
		t.Fatalf("inserting expired session: %v", err)
	}

	if _, err := store.GetUserBySession(ctx, token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetUserBySession(expired): got %v, want ErrSessionNotFound", err)
	}
}

func TestStore_SetBcpUserID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	u, err := store.UpsertUserFromGoogle(ctx, "google-sub-"+uniqueID(t), "d@example.com", "Dave", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}

	readBack := func() string {
		t.Helper()
		var bcpUserID string
		if err := store.pool.QueryRow(ctx,
			`SELECT COALESCE(bcp_user_id, '') FROM users WHERE id = $1`, u.ID,
		).Scan(&bcpUserID); err != nil {
			t.Fatalf("reading back bcp_user_id: %v", err)
		}
		return bcpUserID
	}

	if got := readBack(); got != "" {
		t.Fatalf("bcp_user_id before linking = %q, want empty", got)
	}

	if err := store.SetBcpUserID(ctx, u.ID, "bcp-123"); err != nil {
		t.Fatalf("SetBcpUserID (link): %v", err)
	}
	if got := readBack(); got != "bcp-123" {
		t.Fatalf("bcp_user_id after linking = %q, want %q", got, "bcp-123")
	}

	// Unlinking (empty string) should clear it back to NULL, not store
	// a literal empty string — see SetBcpUserID's NULLIF.
	if err := store.SetBcpUserID(ctx, u.ID, ""); err != nil {
		t.Fatalf("SetBcpUserID (unlink): %v", err)
	}
	if got := readBack(); got != "" {
		t.Fatalf("bcp_user_id after unlinking = %q, want empty", got)
	}
}

func TestStore_Follows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := uniqueID(t)
	u, err := store.UpsertUserFromGoogle(ctx, "google-sub-"+runID, "e@example.com", "Erin", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}
	eventID := "evt-" + runID

	if err := store.AddFollow(ctx, u.ID, eventID, "team", "team-1", "Team One"); err != nil {
		t.Fatalf("AddFollow: %v", err)
	}
	// Idempotent: following the same thing again just refreshes its
	// label, rather than erroring or creating a duplicate row.
	if err := store.AddFollow(ctx, u.ID, eventID, "team", "team-1", "Team One Renamed"); err != nil {
		t.Fatalf("AddFollow (re-add): %v", err)
	}

	follows, err := store.ListFollows(ctx, u.ID, eventID)
	if err != nil {
		t.Fatalf("ListFollows: %v", err)
	}
	if len(follows) != 1 || follows[0].Label != "Team One Renamed" {
		t.Fatalf("ListFollows = %+v, want one follow labeled %q", follows, "Team One Renamed")
	}

	// A follow recorded against a different event shouldn't leak into
	// this one's list — per-event isolation is the whole point of the
	// composite key.
	otherEventID := "evt-other-" + runID
	if err := store.AddFollow(ctx, u.ID, otherEventID, "player", "p-1", "Player One"); err != nil {
		t.Fatalf("AddFollow (other event): %v", err)
	}
	if follows, err = store.ListFollows(ctx, u.ID, eventID); err != nil {
		t.Fatalf("ListFollows: %v", err)
	} else if len(follows) != 1 {
		t.Fatalf("ListFollows leaked a follow from another event: %+v", follows)
	}

	if err := store.RemoveFollow(ctx, u.ID, eventID, "team", "team-1"); err != nil {
		t.Fatalf("RemoveFollow: %v", err)
	}
	if follows, err = store.ListFollows(ctx, u.ID, eventID); err != nil {
		t.Fatalf("ListFollows: %v", err)
	} else if len(follows) != 0 {
		t.Fatalf("ListFollows after remove = %+v, want none", follows)
	}

	// Removing something never followed isn't an error — same rationale
	// as DeleteSession.
	if err := store.RemoveFollow(ctx, u.ID, eventID, "team", "never-followed"); err != nil {
		t.Fatalf("RemoveFollow (never followed): %v", err)
	}
}

func TestStore_RecentEvents_TrimsToMax(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := uniqueID(t)
	u, err := store.UpsertUserFromGoogle(ctx, "google-sub-"+runID, "f@example.com", "Frank", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}

	// Record more than MaxRecentEvents, each with a distinct
	// last_viewed_at (a short sleep between each) so ordering is
	// unambiguous rather than relying on timestamp ties.
	total := MaxRecentEvents + 3
	for i := 0; i < total; i++ {
		eventID := fmt.Sprintf("evt-%s-%d", runID, i)
		if err := store.RecordRecentEvent(ctx, u.ID, eventID, fmt.Sprintf("Event %d", i), false); err != nil {
			t.Fatalf("RecordRecentEvent(%d): %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	events, err := store.ListRecentEvents(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListRecentEvents: %v", err)
	}
	if len(events) != MaxRecentEvents {
		t.Fatalf("ListRecentEvents returned %d events, want %d (MaxRecentEvents)", len(events), MaxRecentEvents)
	}
	wantMostRecent := fmt.Sprintf("evt-%s-%d", runID, total-1)
	if events[0].EventID != wantMostRecent {
		t.Fatalf("most recent event = %q, want %q (list should be newest-first)", events[0].EventID, wantMostRecent)
	}

	// Re-recording one of the already-trimmed-away events bumps it back
	// to the front without growing the list past the cap.
	bumpEventID := fmt.Sprintf("evt-%s-%d", runID, 1)
	if err := store.RecordRecentEvent(ctx, u.ID, bumpEventID, "Bumped", false); err != nil {
		t.Fatalf("RecordRecentEvent (bump): %v", err)
	}
	if events, err = store.ListRecentEvents(ctx, u.ID); err != nil {
		t.Fatalf("ListRecentEvents: %v", err)
	} else if len(events) != MaxRecentEvents {
		t.Fatalf("ListRecentEvents after bump returned %d events, want %d", len(events), MaxRecentEvents)
	} else if events[0].EventID != bumpEventID {
		t.Fatalf("most recent after bump = %q, want %q", events[0].EventID, bumpEventID)
	}
}

func TestStore_AccessControl(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := uniqueID(t)
	u, err := store.UpsertUserFromGoogle(ctx, "google-sub-"+runID, "g@example.com", "Grace", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle: %v", err)
	}
	if u.Role != RoleUser || u.Status != StatusPending {
		t.Fatalf("new user = role %q status %q, want %q/%q", u.Role, u.Status, RoleUser, StatusPending)
	}

	if err := store.SetStatus(ctx, u.ID, StatusApproved); err != nil {
		t.Fatalf("SetStatus (approve): %v", err)
	}
	if err := store.SetRole(ctx, u.ID, RoleAdmin); err != nil {
		t.Fatalf("SetRole (promote): %v", err)
	}

	// GetUserBySession is what every gated route actually reads — the
	// real proof SetStatus/SetRole took effect where it matters, not
	// just a raw column read.
	token, err := store.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := store.GetUserBySession(ctx, token)
	if err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	}
	if got.Role != RoleAdmin || got.Status != StatusApproved {
		t.Fatalf("after promote+approve, GetUserBySession = role %q status %q, want %q/%q", got.Role, got.Status, RoleAdmin, StatusApproved)
	}

	if err := store.SetStatus(ctx, u.ID, StatusRejected); err != nil {
		t.Fatalf("SetStatus (reject): %v", err)
	}
	if got, err = store.GetUserBySession(ctx, token); err != nil {
		t.Fatalf("GetUserBySession: %v", err)
	} else if got.Status != StatusRejected {
		t.Fatalf("status after reject = %q, want %q", got.Status, StatusRejected)
	}

	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var found bool
	for _, listed := range users {
		if listed.ID == u.ID {
			found = true
			if listed.Role != RoleAdmin || listed.Status != StatusRejected {
				t.Errorf("ListUsers entry for this user = role %q status %q, want %q/%q", listed.Role, listed.Status, RoleAdmin, StatusRejected)
			}
		}
	}
	if !found {
		t.Fatalf("ListUsers didn't include user %d", u.ID)
	}
}
