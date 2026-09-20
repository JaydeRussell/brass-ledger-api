//go:build integration

// These exercise *Store against a real Postgres, for the same reason
// internal/user's tests do: it's a thin wrapper over SQL, and the parts
// worth testing (a partial-match ANY() read, an age bound read off
// cached_at, an upsert) are exactly the parts a fake would have to
// reimplement to fake. Gated behind the "integration" build tag so plain
// `go test ./...` never needs DATABASE_URL. Run explicitly with:
//
//	go test -tags=integration ./internal/bcpcache/...
//
// or `make test-integration`, which covers this package and
// internal/user together.
package bcpcache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/db"
)

const testVersion = 1

type payload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// uniqueKey scopes every test's rows to itself. Same approach as
// internal/user's integration tests: no truncation, no per-test
// transaction, so these stay safe to run in parallel and safe to re-run
// against a persistent local database that was never wiped.
func uniqueKey(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("test:%s:%d:%s", t.Name(), time.Now().UnixNano(), suffix)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL must be set to run integration tests — see this file's build-tag comment")
	}
	ctx := context.Background()
	pool, err := db.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connecting to database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	return New(pool)
}

func TestStore_SetThenGet(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := uniqueKey(t, "a")

	if err := store.Set(ctx, key, testVersion, payload{Name: "first", Count: 1}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var got payload
	found, err := store.Get(ctx, key, testVersion, &got)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || got.Name != "first" || got.Count != 1 {
		t.Errorf("Get = %+v (found=%v), want {first 1}", got, found)
	}

	// Set is an upsert — a second write replaces rather than conflicting.
	if err := store.Set(ctx, key, testVersion, payload{Name: "second", Count: 2}); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	if _, err := store.Get(ctx, key, testVersion, &got); err != nil {
		t.Fatalf("Get after upsert: %v", err)
	}
	if got.Name != "second" {
		t.Errorf("Get after upsert = %+v, want the second value", got)
	}
}

func TestStore_GetMissAndVersionMismatch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := uniqueKey(t, "a")

	var got payload
	found, err := store.Get(ctx, uniqueKey(t, "absent"), testVersion, &got)
	if err != nil {
		t.Fatalf("Get on a missing key: %v", err)
	}
	if found {
		t.Error("Get reported found=true for a key that was never written")
	}

	if err := store.Set(ctx, key, testVersion, payload{Name: "v1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	found, err = store.Get(ctx, key, testVersion+1, &got)
	if err != nil {
		t.Fatalf("Get at a newer version: %v", err)
	}
	if found {
		t.Error("Get reported found=true across a version mismatch — bumping CacheSchemaVersion must bust every row")
	}
}

func TestStore_GetFreshHonoursMaxAge(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := uniqueKey(t, "a")

	before := time.Now()
	if err := store.Set(ctx, key, testVersion, payload{Name: "fresh"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var got payload
	found, cachedAt, err := store.GetFresh(ctx, key, testVersion, time.Hour, &got)
	if err != nil {
		t.Fatalf("GetFresh: %v", err)
	}
	if !found || got.Name != "fresh" {
		t.Errorf("GetFresh = %+v (found=%v), want the stored value", got, found)
	}
	// cached_at is the row's real write time, which is what a caller
	// seeds an in-memory entry with — see bcp.Cache.Put.
	if cachedAt.Before(before.Add(-time.Minute)) || cachedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("cachedAt = %s, want roughly now (%s)", cachedAt, before)
	}

	// The same row, read with a maxAge it can't satisfy, reports missing.
	found, _, err = store.GetFresh(ctx, key, testVersion, time.Nanosecond, &got)
	if err != nil {
		t.Fatalf("GetFresh with a tiny maxAge: %v", err)
	}
	if found {
		t.Error("GetFresh reported found=true for a row older than maxAge")
	}

	// A row past maxAge is deliberately left in place, not deleted — a
	// failed refetch is better off serving something stale than nothing.
	if found, err := store.Get(ctx, key, testVersion, &got); err != nil || !found {
		t.Errorf("Get after an expired GetFresh: found=%v err=%v, want the row to still exist", found, err)
	}
}

func TestStore_GetMany(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	present, alsoPresent, absent := uniqueKey(t, "a"), uniqueKey(t, "b"), uniqueKey(t, "c")

	if err := store.Set(ctx, present, testVersion, payload{Name: "a"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(ctx, alsoPresent, testVersion, payload{Name: "b"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	found, err := store.GetMany(ctx, []string{present, alsoPresent, absent}, testVersion)
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("GetMany returned %d rows, want 2 (the absent key must simply not appear)", len(found))
	}
	if string(found[present]) == "" || string(found[alsoPresent]) == "" {
		t.Errorf("GetMany = %v, want raw JSON for both present keys", found)
	}
	if _, ok := found[absent]; ok {
		t.Error("GetMany included a key that was never written")
	}

	// An empty key set must not hit the database at all, and must not
	// produce a nil map the caller has to guard.
	empty, err := store.GetMany(ctx, nil, testVersion)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("GetMany(nil) = %v, %v; want an empty non-nil map and no error", empty, err)
	}
}

func TestStore_GetManyRespectsVersion(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := uniqueKey(t, "a")

	if err := store.Set(ctx, key, testVersion, payload{Name: "old"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	found, err := store.GetMany(ctx, []string{key}, testVersion+1)
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(found) != 0 {
		t.Error("GetMany returned a row written at a different version")
	}
}

func TestStore_Delete(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := uniqueKey(t, "a")

	if err := store.Set(ctx, key, testVersion, payload{Name: "doomed"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var got payload
	found, err := store.Get(ctx, key, testVersion, &got)
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if found {
		t.Error("the row survived Delete")
	}

	// Deleting something that isn't there is the caller's desired end
	// state either way — see the method's doc comment.
	if err := store.Delete(ctx, uniqueKey(t, "never-written")); err != nil {
		t.Errorf("Delete of a missing key returned %v, want nil", err)
	}
}
