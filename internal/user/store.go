package user

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
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

	// Role is "user" or "admin" — see migration 0007. An admin can
	// approve/reject other accounts and promote/demote roles (see
	// internal/api/admin.go); a plain "user" can't, regardless of
	// Status below.
	Role string

	// Status is "pending", "approved", or "rejected" (migration 0007).
	// A valid session alone (RoleUser or RoleAdmin, any status) is
	// enough to authenticate — e.g. to see your own /api/me — but
	// api.RequireApproved additionally requires Status == "approved"
	// before a route does anything real. New accounts default to
	// "pending"; see ADMIN_EMAILS (internal/config) for the one way a
	// sign-in bypasses that default (auto-approved, every sign-in, not
	// just its first).
	Status string

	// AccentTheme is one of ValidAccentThemes (migration 0009) — the
	// frontend's accent-color theme choice (see brass-ledger-web's
	// app/lib/theme.ts's AccentTheme type, which this mirrors exactly).
	// Defaults to "brass" for every account.
	AccentTheme string

	// DossierPublic is whether this account's player dossier (migration
	// 0014) is reachable by anyone at GET /api/players/:bcpUserId/dossier
	// — see internal/api/dossier.go. Defaults to true; SetDossierPublic
	// is the only way to turn it off.
	DossierPublic bool
}

// RoleAdmin and RoleUser are Role's two valid values (also enforced by
// migration 0007's CHECK constraint). StatusPending/StatusApproved/
// StatusRejected are Status's three.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// ValidAccentThemes are AccentTheme's valid values (also enforced by
// migration 0009's CHECK constraint) — a slice rather than 12 named
// constants like RoleAdmin/RoleUser above, since there are too many of
// these for that to stay readable; IsValidAccentTheme below is the
// actual validation entry point callers use.
var ValidAccentThemes = []string{
	"brass", "ultramarine", "sanguine", "verdant", "plague-bloom",
	"necron-emerald", "waaagh", "amethyst", "hive-bloom", "tau-cyan",
	"custodian-gold", "khorne-crimson",
}

// IsValidAccentTheme reports whether theme is one of ValidAccentThemes —
// used by internal/api/me.go's SetAccentTheme to reject a bad value
// before it reaches the database (migration 0009's CHECK constraint is
// the actual backstop, same division of labor as SetStatus/SetRole).
func IsValidAccentTheme(theme string) bool {
	for _, v := range ValidAccentThemes {
		if v == theme {
			return true
		}
	}
	return false
}

// ErrSessionNotFound is returned by GetUserBySession both when the
// token matches no row and when it matches an expired one — callers
// should treat both the same way (not signed in), not distinguish them.
var ErrSessionNotFound = errors.New("session not found or expired")

// Store is this service's user/session persistence, backed by Postgres.
// Its methods satisfy internal/api's userStore interface, which is what
// AuthHandler and friends actually depend on — that's what lets their
// routes be tested against an in-memory fake instead of a real database.
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
// Deliberately never touches role/status on conflict — those default
// to RoleUser/StatusPending on the *first* insert (migration 0007) and
// stay whatever they were set to (by an admin, or by ADMIN_EMAILS
// auto-approval — see internal/api/auth.go's Callback) on every
// subsequent sign-in. A profile refresh should never silently reset
// someone's access.
//
// The returned bool is true iff this call inserted a brand-new row
// (false for a conflict-triggered update) — Postgres's `xmax = 0`
// system-column trick, checked in the same RETURNING round-trip rather
// than a second query. True at most once ever per googleSub; used by
// Callback to email admins about a new pending signup without
// re-notifying on that account's later sign-ins.
func (s *Store) UpsertUserFromGoogle(ctx context.Context, googleSub, email, name, avatarURL string) (User, bool, error) {
	var u User
	var inserted bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (google_sub, email, name, avatar_url, last_login_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (google_sub) DO UPDATE
			SET email = EXCLUDED.email,
				name = EXCLUDED.name,
				avatar_url = EXCLUDED.avatar_url,
				last_login_at = now()
		RETURNING id, email, name, avatar_url, COALESCE(bcp_user_id, ''), role, status, accent_theme, dossier_public, (xmax = 0) AS inserted
	`, googleSub, email, name, avatarURL).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.BcpUserID, &u.Role, &u.Status, &u.AccentTheme, &u.DossierPublic, &inserted)
	if err != nil {
		return User{}, false, fmt.Errorf("upserting user: %w", err)
	}
	return u, inserted, nil
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
		SELECT u.id, u.email, u.name, u.avatar_url, COALESCE(u.bcp_user_id, ''), u.role, u.status, u.accent_theme, u.dossier_public
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token = $1 AND s.expires_at > now()
	`, token).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.BcpUserID, &u.Role, &u.Status, &u.AccentTheme, &u.DossierPublic)
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

// SetStatus approves/rejects/re-pends an account — see internal/api/
// admin.go, the only caller. The value itself is validated there (an
// admin-only route with a fixed set of accepted actions), not here —
// migration 0007's CHECK constraint is the actual backstop against a
// bad value ever reaching the database.
func (s *Store) SetStatus(ctx context.Context, userID int64, status string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET status = $1 WHERE id = $2`,
		status, userID,
	); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}
	return nil
}

// SetRole promotes/demotes an account between RoleUser and RoleAdmin —
// see internal/api/admin.go, the only caller.
func (s *Store) SetRole(ctx context.Context, userID int64, role string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET role = $1 WHERE id = $2`,
		role, userID,
	); err != nil {
		return fmt.Errorf("setting role: %w", err)
	}
	return nil
}

// SetAccentTheme updates a signed-in account's saved accent-color theme
// — see internal/api/me.go, the only caller. Like SetStatus/SetRole
// above, the value itself is validated by the caller (IsValidAccentTheme);
// migration 0009's CHECK constraint is the actual backstop against a bad
// value ever reaching the database.
func (s *Store) SetAccentTheme(ctx context.Context, userID int64, accentTheme string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET accent_theme = $1 WHERE id = $2`,
		accentTheme, userID,
	); err != nil {
		return fmt.Errorf("setting accent_theme: %w", err)
	}
	return nil
}

// SetDossierPublic turns a signed-in account's player dossier visibility
// on/off — see internal/api/dossier.go's SetDossierVisibility, the only
// caller, and User.DossierPublic's doc comment.
func (s *Store) SetDossierPublic(ctx context.Context, userID int64, public bool) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET dossier_public = $1 WHERE id = $2`,
		public, userID,
	); err != nil {
		return fmt.Errorf("setting dossier_public: %w", err)
	}
	return nil
}

// ErrUserNotFound is returned by GetUserByBcpUserID when no account is
// linked to the given BCP user id.
var ErrUserNotFound = errors.New("user not found")

// GetUserByBcpUserID looks up the account (if any) linked to a Best
// Coast Pairings user id — the reverse of the manual link SetBcpUserID
// records. Used wherever a caller has a bcpUserId in hand (a roster
// entry, a pairing, a dossier link) and needs to know whether it maps
// to a Brass Ledger account at all: today, GET /api/players/:bcpUserId/
// dossier (internal/api/dossier.go) and FriendsHandler.SendRequest/
// Events (internal/api/friends.go) — resolving who to send a friend
// request to, and whose events a friendship unlocks. bcp_user_id is
// unique per account in practice (each is set by that account's own
// owner pasting their own profile), though nothing in the schema
// enforces that today, so this returns whichever row matches first.
func (s *Store) GetUserByBcpUserID(ctx context.Context, bcpUserID string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, name, avatar_url, COALESCE(bcp_user_id, ''), role, status, accent_theme, dossier_public
		FROM users
		WHERE bcp_user_id = $1
		LIMIT 1
	`, bcpUserID).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.BcpUserID, &u.Role, &u.Status, &u.AccentTheme, &u.DossierPublic)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, fmt.Errorf("looking up user by bcp_user_id: %w", err)
	}
	return u, nil
}

// DefaultUsersPageSize/MaxUsersPageSize bound ListUsersOptions.PageSize —
// see ListUsers.
const (
	DefaultUsersPageSize = 5
	MaxUsersPageSize     = 100
)

// ListUsersOptions filters/paginates ListUsers. Status is "" or "all"
// for no status filter, else one of StatusPending/StatusApproved/
// StatusRejected. Search is "" for no name/email filter. Page is
// 1-based; anything less than 1 is treated as 1. PageSize is clamped to
// [1, MaxUsersPageSize], defaulting to DefaultUsersPageSize when 0.
type ListUsersOptions struct {
	Status   string
	Search   string
	Page     int
	PageSize int
}

// UserStatusCounts totals every account by status, independent of any
// Search/Status filter — what the admin panel's tab labels ("Pending
// (56)") need regardless of what's currently paged/searched.
type UserStatusCounts struct {
	All      int
	Pending  int
	Approved int
	Rejected int
}

// ListUsersResult is ListUsers' return value: one page of accounts
// matching opts, the total count of accounts matching opts (for the
// caller to compute page count), and the unfiltered per-status totals.
type ListUsersResult struct {
	Items  []User
	Total  int
	Counts UserStatusCounts
}

// ListUsers returns one page of accounts for the admin panel
// (internal/api/admin.go), filtered by opts.Status/opts.Search — within
// the "all" status filter (or no filter), pending accounts first (what
// an admin actually needs to act on), then approved, then rejected;
// newest first within each group and when a single status is filtered.
// Paginated server-side (opts.Page/opts.PageSize) since a live event's
// account list can run to several dozen pending sign-ups alone.
func (s *Store) ListUsers(ctx context.Context, opts ListUsersOptions) (ListUsersResult, error) {
	page := opts.Page
	if page < 1 {
		page = 1
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = DefaultUsersPageSize
	}
	if pageSize > MaxUsersPageSize {
		pageSize = MaxUsersPageSize
	}

	var result ListUsersResult
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status = 'approved'),
		       count(*) FILTER (WHERE status = 'rejected')
		FROM users
	`).Scan(&result.Counts.All, &result.Counts.Pending, &result.Counts.Approved, &result.Counts.Rejected); err != nil {
		return ListUsersResult{}, fmt.Errorf("counting users: %w", err)
	}

	var where []string
	var args []any
	if opts.Status != "" && opts.Status != "all" {
		args = append(args, opts.Status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if opts.Search != "" {
		args = append(args, "%"+opts.Search+"%")
		where = append(where, fmt.Sprintf("(name ILIKE $%d OR email ILIKE $%d)", len(args), len(args)))
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	countQuery := "SELECT count(*) FROM users " + whereClause
	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&result.Total); err != nil {
		return ListUsersResult{}, fmt.Errorf("counting filtered users: %w", err)
	}

	args = append(args, pageSize, (page-1)*pageSize)
	itemsQuery := fmt.Sprintf(`
		SELECT id, email, name, avatar_url, COALESCE(bcp_user_id, ''), role, status
		FROM users
		%s
		ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'approved' THEN 1 WHEN 'rejected' THEN 2 ELSE 3 END, created_at DESC
		LIMIT $%d OFFSET $%d
	`, whereClause, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, itemsQuery, args...)
	if err != nil {
		return ListUsersResult{}, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()

	result.Items = []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.BcpUserID, &u.Role, &u.Status); err != nil {
			return ListUsersResult{}, fmt.Errorf("scanning user: %w", err)
		}
		result.Items = append(result.Items, u)
	}
	if err := rows.Err(); err != nil {
		return ListUsersResult{}, fmt.Errorf("listing users: %w", err)
	}
	return result, nil
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

// FollowCount is how many distinct accounts follow one team/player
// within one event — an aggregate, not tied to any particular follower's
// identity (see CountFollows).
type FollowCount struct {
	Kind  string
	RefID string
	Count int
}

// CountFollows returns, for every team/player anyone follows within one
// event, how many distinct accounts follow it — the "N people tracking
// this" social-proof feature (internal/api/sync.go's FollowCounts). Pure
// aggregation over user_follows; never exposes *which* accounts, just a
// count, the same way a public vote/like count would.
func (s *Store) CountFollows(ctx context.Context, eventID string) ([]FollowCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, ref_id, count(*)
		FROM user_follows
		WHERE event_id = $1
		GROUP BY kind, ref_id
	`, eventID)
	if err != nil {
		return nil, fmt.Errorf("counting follows: %w", err)
	}
	defer rows.Close()

	counts := []FollowCount{}
	for rows.Next() {
		var c FollowCount
		if err := rows.Scan(&c.Kind, &c.RefID, &c.Count); err != nil {
			return nil, fmt.Errorf("scanning follow count: %w", err)
		}
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting follows: %w", err)
	}
	return counts, nil
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

// GetRoundNote returns a user's private note for one round of one event,
// or "" if they've never saved one (or saved one and then cleared it —
// see SetRoundNote, which deletes rather than stores an empty note).
func (s *Store) GetRoundNote(ctx context.Context, userID int64, eventID string, round int) (string, error) {
	var note string
	err := s.pool.QueryRow(ctx,
		`SELECT note FROM user_round_notes WHERE user_id = $1 AND event_id = $2 AND round = $3`,
		userID, eventID, round,
	).Scan(&note)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("getting round note: %w", err)
	}
	return note, nil
}

// SetRoundNote saves (or, given an empty/whitespace-only note, deletes)
// a user's private note for one round of one event — see
// internal/api/sync.go, the only caller.
func (s *Store) SetRoundNote(ctx context.Context, userID int64, eventID string, round int, note string) error {
	if strings.TrimSpace(note) == "" {
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM user_round_notes WHERE user_id = $1 AND event_id = $2 AND round = $3`,
			userID, eventID, round,
		); err != nil {
			return fmt.Errorf("deleting round note: %w", err)
		}
		return nil
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_round_notes (user_id, event_id, round, note, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id, event_id, round) DO UPDATE
			SET note = EXCLUDED.note, updated_at = now()
	`, userID, eventID, round, note); err != nil {
		return fmt.Errorf("setting round note: %w", err)
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
