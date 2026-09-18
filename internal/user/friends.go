package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// FriendRequestPending/Accepted/Declined are FriendRequest.Status's three
// valid values (also enforced by migration 0014's CHECK constraint).
const (
	FriendRequestPending  = "pending"
	FriendRequestAccepted = "accepted"
	FriendRequestDeclined = "declined"
)

// FriendRequest is one row of migration 0014's friend_requests table,
// with the other side's display name joined in — every real caller
// wants a name to show, not just an id.
type FriendRequest struct {
	ID            int64
	RequesterID   int64
	RequesterName string
	RecipientID   int64
	RecipientName string
	Status        string
	CreatedAt     time.Time
}

// Friend is one accepted friendship, from userID's own point of view —
// the *other* account's id/name/BcpUserID, not which side originally
// sent the request (ListFriends resolves that already).
type Friend struct {
	UserID    int64
	Name      string
	BcpUserID string
}

// ErrFriendRequestAlreadyExists is returned by SendFriendRequest when a
// pending or already-accepted request exists between these two accounts
// in either direction — see migration 0014's partial unique index.
var ErrFriendRequestAlreadyExists = errors.New("a friend request already exists between these accounts")

// ErrFriendRequestNotFound is returned by AcceptFriendRequest/
// DeclineFriendRequest when no *pending* request with that id exists
// for that recipient — covers "wrong id", "not addressed to you", and
// "already responded to" alike, so a caller can't probe for which one
// it was.
var ErrFriendRequestNotFound = errors.New("friend request not found")

// SendFriendRequest creates a pending request from requesterID to
// recipientID. Returns ErrFriendRequestAlreadyExists if a pending or
// accepted request already exists between them in either direction —
// migration 0014's own CHECK constraint separately rejects
// requesterID == recipientID (an accidental self-friend), which the
// caller (internal/api/friends.go) already guards against before this
// is ever reached, so that case isn't specially handled here.
//
// Known gap, accepted rather than engineered around for now: if both
// accounts send each other a request before either accepts, both rows
// exist as two independent pending requests (the partial unique index
// only blocks a *second* request in the *same* direction). Accepting
// either one still creates the friendship correctly; the other simply
// becomes a stale pending request the original sender can no longer
// usefully act on. Revisit only if this turns out to confuse people in
// practice.
func (s *Store) SendFriendRequest(ctx context.Context, requesterID, recipientID int64) (FriendRequest, error) {
	var fr FriendRequest
	err := s.pool.QueryRow(ctx, `
		INSERT INTO friend_requests (requester_id, recipient_id)
		VALUES ($1, $2)
		RETURNING id, requester_id, recipient_id, status, created_at
	`, requesterID, recipientID).Scan(&fr.ID, &fr.RequesterID, &fr.RecipientID, &fr.Status, &fr.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return FriendRequest{}, ErrFriendRequestAlreadyExists
		}
		return FriendRequest{}, fmt.Errorf("sending friend request: %w", err)
	}
	return fr, nil
}

// ListIncomingFriendRequests returns userID's pending incoming requests,
// newest first, with each requester's current display name joined in.
func (s *Store) ListIncomingFriendRequests(ctx context.Context, userID int64) ([]FriendRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT fr.id, fr.requester_id, u.name, fr.recipient_id, fr.status, fr.created_at
		FROM friend_requests fr
		JOIN users u ON u.id = fr.requester_id
		WHERE fr.recipient_id = $1 AND fr.status = $2
		ORDER BY fr.created_at DESC
	`, userID, FriendRequestPending)
	if err != nil {
		return nil, fmt.Errorf("listing incoming friend requests: %w", err)
	}
	defer rows.Close()

	requests := []FriendRequest{}
	for rows.Next() {
		var fr FriendRequest
		if err := rows.Scan(&fr.ID, &fr.RequesterID, &fr.RequesterName, &fr.RecipientID, &fr.Status, &fr.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning friend request: %w", err)
		}
		requests = append(requests, fr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing incoming friend requests: %w", err)
	}
	return requests, nil
}

// respondToFriendRequest is AcceptFriendRequest/DeclineFriendRequest's
// shared body — both are "set this pending request (addressed to
// recipientID) to a terminal status," differing only in which one.
// Scoping the UPDATE to recipientID in the WHERE clause (rather than a
// separate ownership check before updating) means there's no window
// where a caller could act on a request that isn't theirs, and no
// separate "not found" vs. "not yours" distinction to leak either.
func (s *Store) respondToFriendRequest(ctx context.Context, requestID, recipientID int64, newStatus string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE friend_requests
		SET status = $1, responded_at = now()
		WHERE id = $2 AND recipient_id = $3 AND status = $4
	`, newStatus, requestID, recipientID, FriendRequestPending)
	if err != nil {
		return fmt.Errorf("responding to friend request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFriendRequestNotFound
	}
	return nil
}

// AcceptFriendRequest accepts a pending request addressed to
// recipientID — see respondToFriendRequest.
func (s *Store) AcceptFriendRequest(ctx context.Context, requestID, recipientID int64) error {
	return s.respondToFriendRequest(ctx, requestID, recipientID, FriendRequestAccepted)
}

// DeclineFriendRequest declines a pending request addressed to
// recipientID — see respondToFriendRequest.
func (s *Store) DeclineFriendRequest(ctx context.Context, requestID, recipientID int64) error {
	return s.respondToFriendRequest(ctx, requestID, recipientID, FriendRequestDeclined)
}

// ListFriends returns every account userID has an accepted friendship
// with — the *other* side of each accepted friend_requests row,
// regardless of who originally sent it.
func (s *Store) ListFriends(ctx context.Context, userID int64) ([]Friend, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.name, COALESCE(u.bcp_user_id, '')
		FROM friend_requests fr
		JOIN users u ON u.id = CASE WHEN fr.requester_id = $1 THEN fr.recipient_id ELSE fr.requester_id END
		WHERE fr.status = $2 AND (fr.requester_id = $1 OR fr.recipient_id = $1)
		ORDER BY u.name
	`, userID, FriendRequestAccepted)
	if err != nil {
		return nil, fmt.Errorf("listing friends: %w", err)
	}
	defer rows.Close()

	friends := []Friend{}
	for rows.Next() {
		var f Friend
		if err := rows.Scan(&f.UserID, &f.Name, &f.BcpUserID); err != nil {
			return nil, fmt.Errorf("scanning friend: %w", err)
		}
		friends = append(friends, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing friends: %w", err)
	}
	return friends, nil
}

// AreFriends reports whether userID and otherUserID have an accepted
// friendship, in either direction — the gate FriendsHandler.Events uses
// before showing one account another's linked BCP events.
func (s *Store) AreFriends(ctx context.Context, userID, otherUserID int64) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM friend_requests
			WHERE status = $3 AND (
				(requester_id = $1 AND recipient_id = $2) OR
				(requester_id = $2 AND recipient_id = $1)
			)
		)
	`, userID, otherUserID, FriendRequestAccepted).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking friendship: %w", err)
	}
	return exists, nil
}

// RemoveFriend deletes the accepted friendship between userID and
// friendUserID, in whichever direction it exists. Removing a
// friendship that doesn't exist isn't an error — same rationale as
// RemoveFollow/DeleteSession.
func (s *Store) RemoveFriend(ctx context.Context, userID, friendUserID int64) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM friend_requests
		WHERE status = $3 AND (
			(requester_id = $1 AND recipient_id = $2) OR
			(requester_id = $2 AND recipient_id = $1)
		)
	`, userID, friendUserID, FriendRequestAccepted); err != nil {
		return fmt.Errorf("removing friend: %w", err)
	}
	return nil
}
