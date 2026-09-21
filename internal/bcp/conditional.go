package bcp

import (
	"net/http"
	"sync"
)

// Conditional requests against BCP.
//
// BCP sends an ETag on its responses and honours If-None-Match —
// verified against the real API on 2026-09-20: re-requesting an event's
// roster with the ETag it had just returned came back 304 with zero
// bytes instead of 2,144. So every refetch of something that hasn't
// actually changed can cost almost nothing.
//
// This is not a way to make fewer requests to BCP; the request count is
// identical. It makes the requests we cannot avoid dramatically
// cheaper, on both sides, and it matters precisely where the durable
// cache cannot help — an event in progress, its standings, a player's
// history. Those are refetched on a timer forever because their data
// genuinely moves, and most of the time it hasn't moved yet.
//
// It pairs with the stale-while-revalidate decision rather than
// duplicating it: SWR moved most refreshes off the critical path, and
// this makes the one path deliberately left synchronous (a round's
// pairings) cheap instead of stale.

const (
	// maxConditionalEntries bounds how many response bodies are held for
	// revalidation. Bodies are kept, not just ETags: a 304 carries no
	// content, so without the body there is nothing to answer with.
	maxConditionalEntries = 256

	// maxConditionalBodyBytes skips storing a response too large to be
	// worth holding in memory. A handful of events carry enormous
	// organiser descriptions (19 KB of one 20 KB document, measured), and
	// those are exactly the ones worth revalidating — so this is set well
	// above them and exists only to stop something pathological from
	// sitting in memory for the life of the process.
	maxConditionalBodyBytes = 512 * 1024
)

// conditionalEntry is what a previous response left behind so the next
// request for the same URL can ask "has this changed?".
type conditionalEntry struct {
	etag string
	body []byte
}

// conditionalStore holds those, keyed by full request URL rather than by
// cache key.
//
// By URL because that is what an ETag is about, and because it is the
// only key that works uniformly: several fetch functions here make more
// than one request (the roster reads /players and /teamplayers; each
// history feed crawls up to ten pages), so a per-cache-key store would
// have nowhere to put the second one.
type conditionalStore struct {
	mu      sync.Mutex
	entries map[string]conditionalEntry
}

func newConditionalStore() *conditionalStore {
	return &conditionalStore{entries: make(map[string]conditionalEntry)}
}

// validator returns the ETag to send for rawURL, if there is one.
func (s *conditionalStore) validator(rawURL string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[rawURL]
	return e.etag, ok
}

// body returns the stored response for rawURL — what a 304 means.
func (s *conditionalStore) body(rawURL string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[rawURL]
	if !ok {
		return nil, false
	}
	return e.body, true
}

// remember stores a response so the next request for rawURL can be
// conditional. A response with no ETag is not stored: there would be
// nothing to revalidate it with, and holding the body would be pure
// memory cost.
func (s *conditionalStore) remember(rawURL, etag string, body []byte) {
	if etag == "" || len(body) > maxConditionalBodyBytes {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= maxConditionalEntries {
		// Arbitrary eviction. There is no useful recency signal here and
		// the cost of evicting the wrong one is a single full response
		// instead of a 304 — which is what every request cost before
		// this existed.
		for k := range s.entries {
			delete(s.entries, k)
			break
		}
	}
	s.entries[rawURL] = conditionalEntry{etag: etag, body: body}
}

// forget drops rawURL, used when a stored body turns out to be
// unusable. Leaving it would mean answering every future 304 for that
// URL with the same broken content.
func (s *conditionalStore) forget(rawURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, rawURL)
}

// isNotModified reports whether a response says "you already have this".
func isNotModified(res *http.Response) bool {
	return res.StatusCode == http.StatusNotModified
}
