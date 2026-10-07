//go:build integration

// Run with `go test -tags=integration ./internal/follow/...` against a
// real Postgres (DATABASE_URL), as the CI integration job does.
package follow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/db"
	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

type fixture struct {
	store *Store
	users *user.Store
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL must be set to run integration tests")
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
	return fixture{store: New(pool), users: user.NewStore(pool)}
}

// newUser creates an account unique to this run.
func (fx fixture) newUser(t *testing.T, label string) int64 {
	t.Helper()
	sub := fmt.Sprintf("%s-%s-%d", t.Name(), label, time.Now().UnixNano())
	u, _, err := fx.users.UpsertUserFromGoogle(context.Background(), sub, sub+"@example.com", label, "")
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return u.ID
}

func eventID(t *testing.T) string { return fmt.Sprintf("evt-%s-%d", t.Name(), time.Now().UnixNano()) }

func TestEnsureLink_KeepsTokenAndUpdatesExpiry(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	owner := fx.newUser(t, "owner")
	evt := eventID(t)

	first, err := fx.store.EnsureLink(ctx, owner, evt, "p1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	second, err := fx.store.EnsureLink(ctx, owner, evt, "p1b", later)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token != first.Token || second.ID != first.ID {
		t.Errorf("second EnsureLink = %+v, want the same link as %+v", second, first)
	}
	if second.PlayerID != "p1b" || !second.ExpiresAt.Equal(later) {
		t.Errorf("second EnsureLink player/expiry = %s/%v, want p1b/%v", second.PlayerID, second.ExpiresAt, later)
	}

	byToken, err := fx.store.GetLinkByToken(ctx, first.Token)
	if err != nil || byToken.ID != first.ID {
		t.Errorf("GetLinkByToken = %+v, %v", byToken, err)
	}
}

func TestEnsureLink_ReplacesAnExpiredToken(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	owner := fx.newUser(t, "owner")
	evt := eventID(t)

	expired, err := fx.store.EnsureLink(ctx, owner, evt, "p1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.GetLinkByToken(ctx, expired.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token lookup err = %v, want ErrNotFound", err)
	}
	if _, err := fx.store.GetLinkForUser(ctx, owner, evt); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired link for user err = %v, want ErrNotFound", err)
	}

	fresh, err := fx.store.EnsureLink(ctx, owner, evt, "p1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Token == expired.Token {
		t.Error("renewing an expired link kept its old token")
	}
}

func TestSpectating_SaveReplaceListAndCascade(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	owner := fx.newUser(t, "owner")
	fan := fx.newUser(t, "fan")
	linked, picked := eventID(t)+"-a", eventID(t)+"-b"

	link, err := fx.store.EnsureLink(ctx, owner, linked, "p1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.store.SaveSpectating(ctx, fan, Spectated{EventID: linked, PlayerID: "p1", FollowLinkID: &link.ID, ExpiresAt: link.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.SaveSpectating(ctx, fan, Spectated{EventID: picked, PlayerID: "p2", ExpiresAt: time.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Following someone else in the same event replaces the row.
	if err := fx.store.SaveSpectating(ctx, fan, Spectated{EventID: picked, PlayerID: "p3", ExpiresAt: time.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	rows, err := fx.store.ListSpectating(ctx, fan)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].EventID != linked || rows[1].PlayerID != "p3" || rows[1].FollowLinkID != nil {
		t.Fatalf("ListSpectating = %+v", rows)
	}

	if err := fx.store.DeleteLink(ctx, owner, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.GetSpectating(ctx, fan, linked); !errors.Is(err, ErrNotFound) {
		t.Errorf("after revoking, GetSpectating err = %v, want ErrNotFound", err)
	}

	if err := fx.store.DeleteSpectating(ctx, fan, picked); err != nil {
		t.Fatal(err)
	}
	if rows, _ := fx.store.ListSpectating(ctx, fan); len(rows) != 0 {
		t.Errorf("after removing, ListSpectating = %+v", rows)
	}
}

func TestDeleteExpired(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	owner := fx.newUser(t, "owner")
	fan := fx.newUser(t, "fan")
	evt := eventID(t)

	link, err := fx.store.EnsureLink(ctx, owner, evt, "p1", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.store.SaveSpectating(ctx, fan, Spectated{EventID: evt, PlayerID: "p1", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := fx.store.DeleteExpired(ctx); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := fx.store.pool.QueryRow(ctx, `SELECT count(*) FROM follow_links WHERE id = $1`, link.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("expired link still stored (count %d, err %v)", n, err)
	}
	if err := fx.store.pool.QueryRow(ctx, `SELECT count(*) FROM spectating WHERE user_id = $1`, fan).Scan(&n); err != nil || n != 0 {
		t.Errorf("expired spectating row still stored (count %d, err %v)", n, err)
	}
}
