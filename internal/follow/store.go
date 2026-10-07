// Package follow stores follow links and the events each account is
// spectating.
package follow

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

// ErrNotFound covers a missing, revoked and expired link or row alike, so
// callers can't tell them apart.
var ErrNotFound = errors.New("not found")

// Link is one row of follow_links.
type Link struct {
	ID        int64
	Token     string
	UserID    int64
	EventID   string
	PlayerID  string
	ExpiresAt time.Time
}

// Spectated is one row of spectating.
type Spectated struct {
	EventID      string
	PlayerID     string
	FollowLinkID *int64
	ExpiresAt    time.Time
}

// Store is backed by Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const linkColumns = `id, token, user_id, event_id, player_id, expires_at`

func scanLink(row pgx.Row) (Link, error) {
	var l Link
	err := row.Scan(&l.ID, &l.Token, &l.UserID, &l.EventID, &l.PlayerID, &l.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

// EnsureLink returns userID's live link for eventID, creating one if there
// is none. An existing link keeps its token; its player and expiry are
// updated, since the roster entry or the event's dates can change.
func (s *Store) EnsureLink(ctx context.Context, userID int64, eventID, playerID string, expiresAt time.Time) (Link, error) {
	token, err := newToken()
	if err != nil {
		return Link{}, err
	}
	// An expired row still holds the (user, event) slot, so it is replaced
	// with a fresh token rather than revived.
	return scanLink(s.pool.QueryRow(ctx, `
		INSERT INTO follow_links (token, user_id, event_id, player_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, event_id) DO UPDATE SET
			token      = CASE WHEN follow_links.expires_at < now() THEN EXCLUDED.token ELSE follow_links.token END,
			player_id  = EXCLUDED.player_id,
			expires_at = EXCLUDED.expires_at
		RETURNING `+linkColumns,
		token, userID, eventID, playerID, expiresAt))
}

// GetLinkForUser returns userID's live link for eventID.
func (s *Store) GetLinkForUser(ctx context.Context, userID int64, eventID string) (Link, error) {
	return scanLink(s.pool.QueryRow(ctx, `
		SELECT `+linkColumns+` FROM follow_links
		WHERE user_id = $1 AND event_id = $2 AND expires_at > now()
	`, userID, eventID))
}

// GetLinkByToken returns the live link with this token.
func (s *Store) GetLinkByToken(ctx context.Context, token string) (Link, error) {
	return scanLink(s.pool.QueryRow(ctx, `
		SELECT `+linkColumns+` FROM follow_links
		WHERE token = $1 AND expires_at > now()
	`, token))
}

// DeleteLink revokes userID's link for eventID, and with it every
// spectating row that came from it.
func (s *Store) DeleteLink(ctx context.Context, userID int64, eventID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM follow_links WHERE user_id = $1 AND event_id = $2`, userID, eventID)
	return err
}

// SaveSpectating records that userID is spectating playerID in eventID,
// replacing whoever they were following in that event before.
func (s *Store) SaveSpectating(ctx context.Context, userID int64, sp Spectated) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO spectating (user_id, event_id, player_id, follow_link_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, event_id) DO UPDATE SET
			player_id      = EXCLUDED.player_id,
			follow_link_id = EXCLUDED.follow_link_id,
			expires_at     = EXCLUDED.expires_at,
			created_at     = now()
	`, userID, sp.EventID, sp.PlayerID, sp.FollowLinkID, sp.ExpiresAt)
	return err
}

// ListSpectating returns userID's live spectated events, soonest expiry
// first.
func (s *Store) ListSpectating(ctx context.Context, userID int64) ([]Spectated, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, player_id, follow_link_id, expires_at FROM spectating
		WHERE user_id = $1 AND expires_at > now()
		ORDER BY expires_at
	`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Spectated, error) {
		var sp Spectated
		err := row.Scan(&sp.EventID, &sp.PlayerID, &sp.FollowLinkID, &sp.ExpiresAt)
		return sp, err
	})
}

// GetSpectating returns userID's live spectated row for eventID.
func (s *Store) GetSpectating(ctx context.Context, userID int64, eventID string) (Spectated, error) {
	var sp Spectated
	err := s.pool.QueryRow(ctx, `
		SELECT event_id, player_id, follow_link_id, expires_at FROM spectating
		WHERE user_id = $1 AND event_id = $2 AND expires_at > now()
	`, userID, eventID).Scan(&sp.EventID, &sp.PlayerID, &sp.FollowLinkID, &sp.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Spectated{}, ErrNotFound
	}
	return sp, err
}

// DeleteSpectating removes eventID from userID's spectated events.
func (s *Store) DeleteSpectating(ctx context.Context, userID int64, eventID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM spectating WHERE user_id = $1 AND event_id = $2`, userID, eventID)
	return err
}

// DeleteExpired removes every expired link and spectating row. Reads
// already ignore expired rows; this keeps them from accumulating.
func (s *Store) DeleteExpired(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM spectating WHERE expires_at <= now()`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM follow_links WHERE expires_at <= now()`)
	return err
}
