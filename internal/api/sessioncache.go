package api

import (
	"context"
	"sync"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/user"
)

// sessionCacheTTL is how long a resolved session is reused without
// asking Postgres again.
//
// Every gated route resolves the session before doing anything else, so
// this was one Neon round trip per request — six on a home page load,
// before any handler did work. None of those six can disagree with each
// other; they are the same query, same answer, microseconds apart.
//
// Deliberately seconds, not minutes. Sessions are opaque server-side
// tokens specifically so signing out can revoke one immediately rather
// than leaving it valid until its own expiry (see internal/user), and a
// generous TTL here would quietly convert them into JWTs with a 30-
// second grace period. Sign-out doesn't rely on that grace at all —
// DeleteSession drops the entry — but an admin approving an account,
// or any other change to the row, is only guaranteed visible this fast.
const sessionCacheTTL = 30 * time.Second

type sessionCacheEntry struct {
	u        user.User
	resolved time.Time
}

// CachedUserStore is a userStore that remembers session lookups for
// sessionCacheTTL.
//
// It wraps rather than replaces: every method it doesn't name passes
// straight through to the real store, so this stays a caching decorator
// and internal/user stays pure persistence.
//
// The entries hold a whole user.User, not just an id, because that is
// what the middleware needs — role and status included. That makes
// every write to a users row a correctness problem for this cache, so
// each one flushes it (see the overrides below). Flushing everything
// rather than one user's entries is deliberate: those writes are rare
// (linking a BCP profile, an accent colour, an admin decision) while
// reads are every request, and an index from user id back to tokens
// would be more machinery than the thing it saves.
type CachedUserStore struct {
	userStore

	mu      sync.Mutex
	entries map[string]sessionCacheEntry
}

// NewCachedUserStore wraps store with a short-lived session cache.
func NewCachedUserStore(store userStore) *CachedUserStore {
	return &CachedUserStore{
		userStore: store,
		entries:   make(map[string]sessionCacheEntry),
	}
}

// GetUserBySession answers from the cache when the entry is younger
// than sessionCacheTTL, and otherwise asks the real store.
//
// A failed lookup is never cached. "Not signed in" is the answer an
// attacker gets to retry freely and the answer a just-signed-in user
// needs to stop getting; neither is improved by remembering it.
func (s *CachedUserStore) GetUserBySession(ctx context.Context, token string) (user.User, error) {
	s.mu.Lock()
	entry, ok := s.entries[token]
	s.mu.Unlock()
	if ok && time.Since(entry.resolved) < sessionCacheTTL {
		return entry.u, nil
	}

	u, err := s.userStore.GetUserBySession(ctx, token)
	if err != nil {
		return user.User{}, err
	}

	s.mu.Lock()
	s.entries[token] = sessionCacheEntry{u: u, resolved: time.Now()}
	s.mu.Unlock()
	return u, nil
}

// DeleteSession drops the cached entry as well as the row, so signing
// out takes effect on the next request rather than at the end of a TTL.
// This is the property that lets the cache exist at all.
func (s *CachedUserStore) DeleteSession(ctx context.Context, token string) error {
	err := s.userStore.DeleteSession(ctx, token)
	s.mu.Lock()
	delete(s.entries, token)
	s.mu.Unlock()
	return err
}

// flush empties the cache. Called after any write that changes a users
// row — after, not before, so a read racing the write can't repopulate
// the stale value and survive the flush.
func (s *CachedUserStore) flush() {
	s.mu.Lock()
	s.entries = make(map[string]sessionCacheEntry)
	s.mu.Unlock()
}

func (s *CachedUserStore) UpsertUserFromGoogle(ctx context.Context, googleSub, email, name, avatarURL string) (user.User, bool, error) {
	u, created, err := s.userStore.UpsertUserFromGoogle(ctx, googleSub, email, name, avatarURL)
	s.flush()
	return u, created, err
}

func (s *CachedUserStore) SetBcpUserID(ctx context.Context, userID int64, bcpUserID string) error {
	err := s.userStore.SetBcpUserID(ctx, userID, bcpUserID)
	s.flush()
	return err
}

func (s *CachedUserStore) SetStatus(ctx context.Context, userID int64, status string) error {
	err := s.userStore.SetStatus(ctx, userID, status)
	s.flush()
	return err
}

func (s *CachedUserStore) SetRole(ctx context.Context, userID int64, role string) error {
	err := s.userStore.SetRole(ctx, userID, role)
	s.flush()
	return err
}

func (s *CachedUserStore) SetAccentTheme(ctx context.Context, userID int64, accentTheme string) error {
	err := s.userStore.SetAccentTheme(ctx, userID, accentTheme)
	s.flush()
	return err
}

func (s *CachedUserStore) SetDossierPublic(ctx context.Context, userID int64, public bool) error {
	err := s.userStore.SetDossierPublic(ctx, userID, public)
	s.flush()
	return err
}
