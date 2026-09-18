//go:build integration

// See store_integration_test.go's own doc comment for why these tests
// exist and how to run them (`go test -tags=integration ./internal/user/...`
// against a real Postgres — `make test-integration` locally).
package user

import (
	"context"
	"errors"
	"testing"
)

// TestStore_GetUserByBcpUserID lives in store_integration_test.go (added
// there by the public-dossiers PR, merged ahead of this one) — not
// redefined here.

func TestStore_FriendRequestLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := uniqueID(t)
	a, _, err := store.UpsertUserFromGoogle(ctx, "google-sub-a-"+runID, "a@example.com", "Anna", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle (a): %v", err)
	}
	b, _, err := store.UpsertUserFromGoogle(ctx, "google-sub-b-"+runID, "b@example.com", "Bea", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle (b): %v", err)
	}

	fr, err := store.SendFriendRequest(ctx, a.ID, b.ID)
	if err != nil {
		t.Fatalf("SendFriendRequest: %v", err)
	}
	if fr.Status != FriendRequestPending {
		t.Fatalf("new request status = %q, want %q", fr.Status, FriendRequestPending)
	}

	// A second request in the same direction while the first is still
	// pending is rejected — migration 0014's partial unique index.
	if _, err := store.SendFriendRequest(ctx, a.ID, b.ID); !errors.Is(err, ErrFriendRequestAlreadyExists) {
		t.Fatalf("duplicate SendFriendRequest = %v, want %v", err, ErrFriendRequestAlreadyExists)
	}

	incoming, err := store.ListIncomingFriendRequests(ctx, b.ID)
	if err != nil {
		t.Fatalf("ListIncomingFriendRequests: %v", err)
	}
	if len(incoming) != 1 || incoming[0].ID != fr.ID || incoming[0].RequesterName != "Anna" {
		t.Fatalf("incoming = %+v, want one request from Anna", incoming)
	}

	if areFriends, err := store.AreFriends(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("AreFriends: %v", err)
	} else if areFriends {
		t.Fatal("AreFriends before accept = true, want false")
	}

	// The wrong recipient (A, the requester) can't accept it.
	if err := store.AcceptFriendRequest(ctx, fr.ID, a.ID); !errors.Is(err, ErrFriendRequestNotFound) {
		t.Fatalf("AcceptFriendRequest (wrong recipient) = %v, want %v", err, ErrFriendRequestNotFound)
	}

	if err := store.AcceptFriendRequest(ctx, fr.ID, b.ID); err != nil {
		t.Fatalf("AcceptFriendRequest: %v", err)
	}
	// Accepting an already-resolved request fails the same way.
	if err := store.AcceptFriendRequest(ctx, fr.ID, b.ID); !errors.Is(err, ErrFriendRequestNotFound) {
		t.Fatalf("re-AcceptFriendRequest = %v, want %v", err, ErrFriendRequestNotFound)
	}

	if areFriends, err := store.AreFriends(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("AreFriends: %v", err)
	} else if !areFriends {
		t.Fatal("AreFriends after accept = false, want true")
	}
	// Symmetric regardless of argument order.
	if areFriends, err := store.AreFriends(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("AreFriends (reversed): %v", err)
	} else if !areFriends {
		t.Fatal("AreFriends(b, a) after accept = false, want true")
	}

	friendsOfA, err := store.ListFriends(ctx, a.ID)
	if err != nil {
		t.Fatalf("ListFriends (a): %v", err)
	}
	if len(friendsOfA) != 1 || friendsOfA[0].UserID != b.ID || friendsOfA[0].Name != "Bea" {
		t.Fatalf("A's friends = %+v, want one entry for Bea", friendsOfA)
	}

	if err := store.RemoveFriend(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("RemoveFriend: %v", err)
	}
	if areFriends, err := store.AreFriends(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("AreFriends after remove: %v", err)
	} else if areFriends {
		t.Fatal("AreFriends after remove = true, want false")
	}
	// Removing a friendship that no longer exists isn't an error.
	if err := store.RemoveFriend(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("RemoveFriend (already gone): %v", err)
	}
}

func TestStore_FriendRequestDeclineThenResend(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	runID := uniqueID(t)
	a, _, err := store.UpsertUserFromGoogle(ctx, "google-sub-a-"+runID, "a@example.com", "Anna", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle (a): %v", err)
	}
	b, _, err := store.UpsertUserFromGoogle(ctx, "google-sub-b-"+runID, "b@example.com", "Bea", "")
	if err != nil {
		t.Fatalf("UpsertUserFromGoogle (b): %v", err)
	}

	fr, err := store.SendFriendRequest(ctx, a.ID, b.ID)
	if err != nil {
		t.Fatalf("SendFriendRequest: %v", err)
	}
	if err := store.DeclineFriendRequest(ctx, fr.ID, b.ID); err != nil {
		t.Fatalf("DeclineFriendRequest: %v", err)
	}

	// A declined request doesn't block a real one later — the partial
	// unique index only covers pending/accepted.
	if _, err := store.SendFriendRequest(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("SendFriendRequest after decline: %v", err)
	}
}
