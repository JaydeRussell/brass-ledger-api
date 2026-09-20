package bcp

import "time"

// dateLayouts are the two shapes BCP actually publishes dates in: a full
// RFC3339 timestamp for most things, and a bare calendar date for some
// event records. Parsed in this order, first match wins.
var dateLayouts = []string{time.RFC3339, "2006-01-02"}

// ParseDate parses whichever of dateLayouts matches, or reports ok =
// false for an empty or unrecognized string. BCP not publishing a date
// isn't an error — it's a "can't tell" for whatever the caller was
// trying to decide, and every caller here treats it as such rather than
// failing the request.
//
// Lives in this package rather than internal/api because the format is
// BCP's, not ours, and the cache-policy code below needs it too — see
// eventInfoTTL in client.go.
func ParseDate(s string) (t time.Time, ok bool) {
	for _, layout := range dateLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
