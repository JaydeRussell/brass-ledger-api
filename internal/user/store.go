package user

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionDuration is how long a session stays valid after it's created.
// There's no sliding-expiration/refresh yet — signing in again is
// currently the only way to extend a session past this.
const SessionDuration = 30 * 24 * time.Hour

// User is one signed-in account, as stored in Postgres.
type User struct {
	ID        int64
	Email     string
	Name      string
	AvatarURL string
	// BcpUserID is the Best Coast Pairings user id this account has been
	// manually linked to (see SetBcpUserID), or "" if unlinked. Nothing
	// links these automatically — BCP's own API has no email field to
	// match against (see internal/bcp's FetchPlayerEventHistory doc
	// comment) — so this is set once by the user pasting their own BCP
	// profile URL/id.
	BcpUserID string
}

// ErrSessionNotFound is returned by GetUserBySession both when the
// token matches no row and when it matches an expired one — callers
// should treat both the same way (not signed in), not distinguish them.
var ErrSessionNotFound = errors.New("session not found or expired")

// Store is this service's user/session persistence, backed by Postgres.
// Its methods satisfy internal/api's userStore interface, which is what
// RegisterAuthRoutes actually depends on — that's what lets the routes
// be tested against an in-memory fake instead of a real database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing connection pool (see internal/db) as a
// Store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertUserFromGoogle creates a user for this Google account on first
// sign-in, or refreshes their profile (name/avatar can change on
// Google's side) and last_login_at on every subsequent one. googleSub
// (not email) is the stable identity being matched on — see the users
// table migration's comment. Takes plain fields rather than an
// auth.UserInfo so this package doesn't need to know anything about
// Google specifically — internal/api's routes are what translate one
// into the other.
func (s *Store) UpsertUserFromGoogle(ctx context.Context, googleSub, email, name, avatarURL string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (google_sub, email, name, avatar_url, last_login_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (google_sub) DO UPDATE
			SET email = EXCLUDED.email,
				name = EXCLUDED.name,
				avatar_url = EXCLUDED.avatar_url,
				last_login_at = now()
		RETURNING id, email, name, avatar_url, COALESCE(bcp_user_id, '')
	`, googleSub, email, name, avatarURL).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.BcpUserID)
	if err != nil {
		return User{}, fmt.Errorf("upserting user: %w", err)
	}
	return u, nil
}

// CreateSession issues a new session for a user and returns its opaque
// token — what the caller sets as the session cookie's value.
func (s *Store) CreateSession(ctx context.Context, userID int64) (string, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3)`,
		token, userID, time.Now().Add(SessionDuration),
	); err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}
	return token, nil
}

// GetUserBySession returns the user a non-expired session token belongs
// to, or ErrSessionNotFound.
func (s *Store) GetUserBySession(ctx context.Context, token string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.avatar_url, COALESCE(u.bcp_user_id, '')
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token = $1 AND s.expires_at > now()
	`, token).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.BcpUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrSessionNotFound
		}
		return User{}, fmt.Errorf("looking up session: %w", err)
	}
	return u, nil
}

// DeleteSession revokes a session (sign-out). Deleting a token that
// doesn't exist isn't an error — the caller's desired end state (no
// such session) already holds either way.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token = $1`, token); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// SetBcpUserID links (or, given "", unlinks) a Best Coast Pairings user
// id to an account — see User.BcpUserID's doc comment for why this is a
// manual, one-time step rather than something resolved automatically.
func (s *Store) SetBcpUserID(ctx context.Context, userID int64, bcpUserID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET bcp_user_id = NULLIF($1, '') WHERE id = $2`,
		bcpUserID, userID,
	); err != nil {
		return fmt.Errorf("setting bcp_user_id: %w", err)
	}
	return nil
}

// MaxRecentEvents mirrors the frontend's own trim limit (see
// brass-ledger-web's app/lib/recentEvents.ts MAX_RECENT_EVENTS) —
// kept here too so the server-side list is trimmed the same way
// regardless of which client wrote to it.
const MaxRecentEvents = 8

// Follow is one followed team/player within one event — see the
// migration 0003 comment for how this mirrors the frontend's Followed
// union.
type Follow struct {
	Kind  string // "team" or "player"
	RefID string
	Label string
}

// RecentEvent is one entry in a user's recently-viewed-events list.
type RecentEvent struct {
	EventID      string
	EventName    string
	TeamEvent    bool
	LastViewedAt time.Time
}

// ListFollows returns everything a user follows within one event, in no
// particular guaranteed order (the frontend re-sorts/displays as needed).
func (s *Store) ListFollows(ctx context.Context, userID int64, eventID string) ([]Follow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT kind, ref_id, label FROM user_follows WHERE user_id = $1 AND event_id = $2`,
		userID, eventID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing follows: %w", err)
	}
	defer rows.Close()

	follows := []Follow{}
	for rows.Next() {
		var f Follow
		if err := rows.Scan(&f.Kind, &f.RefID, &f.Label); err != nil {
			return nil, fmt.Errorf("scanning follow: %w", err)
		}
		follows = append(follows, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing follows: %w", err)
	}
	return follows, nil
}

// AddFollow records that a user follows a team/player within an event.
// Idempotent — following something already followed just refreshes its
// label (e.g. if BCP's own display name for it changed since).
func (s *Store) AddFollow(ctx context.Context, userID int64, eventID, kind, refID, label string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_follows (user_id, event_id, kind, ref_id, label)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, event_id, kind, ref_id) DO UPDATE SET label = EXCLUDED.label
	`, userID, eventID, kind, refID, label); err != nil {
		return fmt.Errorf("adding follow: %w", err)
	}
	return nil
}

// RemoveFollow un-follows a team/player within an event. Removing
// something not currently followed isn't an error — same rationale as
// DeleteSession.
func (s *Store) RemoveFollow(ctx context.Context, userID int64, eventID, kind, refID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM user_follows WHERE user_id = $1 AND event_id = $2 AND kind = $3 AND ref_id = $4`,
		userID, eventID, kind, refID,
	); err != nil {
		return fmt.Errorf("removing follow: %w", err)
	}
	return nil
}

// ListRecentEvents returns a user's recently-viewed events, most recent
// first, already capped at MaxRecentEvents.
func (s *Store) ListRecentEvents(ctx context.Context, userID int64) ([]RecentEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, event_name, team_event, last_viewed_at
		FROM user_recent_events
		WHERE user_id = $1
		ORDER BY last_viewed_at DESC
		LIMIT $2
	`, userID, MaxRecentEvents)
	if err != nil {
		return nil, fmt.Errorf("listing recent events: %w", err)
	}
	defer rows.Close()

	events := []RecentEvent{}
	for rows.Next() {
		var e RecentEvent
		if err := rows.Scan(&e.EventID, &e.EventName, &e.TeamEvent, &e.LastViewedAt); err != nil {
			return nil, fmt.Errorf("scanning recent event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing recent events: %w", err)
	}
	return events, nil
}

// RecordRecentEvent upserts an event into a user's recently-viewed list,
// bumping it to the front (last_viewed_at = now()), then trims anything
// past MaxRecentEvents — mirroring recentEvents.ts's client-side
// recordRecentEvent/slice(0, MAX_RECENT_EVENTS), just enforced here too
// so it holds regardless of which device wrote most recently.
func (s *Store) RecordRecentEvent(ctx context.Context, userID int64, eventID, eventName string, teamEvent bool) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_recent_events (user_id, event_id, event_name, team_event, last_viewed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id, event_id) DO UPDATE
			SET event_name = EXCLUDED.event_name,
				team_event = EXCLUDED.team_event,
				last_viewed_at = now()
	`, userID, eventID, eventName, teamEvent); err != nil {
		return fmt.Errorf("recording recent event: %w", err)
	}

	if _, err := s.pool.Exec(ctx, `
		DELETE FROM user_recent_events
		WHERE user_id = $1 AND event_id NOT IN (
			SELECT event_id FROM user_recent_events
			WHERE user_id = $1
			ORDER BY last_viewed_at DESC
			LIMIT $2
		)
	`, userID, MaxRecentEvents); err != nil {
		return fmt.Errorf("trimming recent events: %w", err)
	}
	return nil
}

func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
