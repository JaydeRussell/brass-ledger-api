package bcp

import (
	"errors"
	"fmt"
	"net/http"
)

// StatusError is BCP answering with a non-2xx status.
//
// It exists so callers can tell "this event will never exist" apart from
// "BCP is having a moment". Everything in this package used to return a
// flat fmt.Errorf for both, which meant the cache had to treat every
// failure as retryable — and a registration pointing at a deleted event
// then cost a real BCP request on every single page load, forever. See
// Cache.Get's negative-caching note.
//
// The message is deliberately unchanged from the string this used to
// format: internal/api's bcpError puts err.Error() straight into a 502
// body, so the wording is user-visible.
type StatusError struct {
	StatusCode int
	URL        string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("BCP request to %s failed: HTTP %d", e.URL, e.StatusCode)
}

// IsGone reports whether err is BCP saying the thing asked for does not
// exist — as opposed to a refusal, a rate limit, or a failure on their
// side.
//
// Only 404 and 410 count. Not 403 (which for this unofficial API is far
// more likely to mean our own request is malformed or unauthenticated
// than that the event is genuinely private), not 429, and nothing in
// 5xx: all of those are things that can be true now and false in a
// minute, and a caller that caches them as permanent would hide real
// data. Network errors aren't StatusErrors at all and so are never gone.
func IsGone(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusGone
}
