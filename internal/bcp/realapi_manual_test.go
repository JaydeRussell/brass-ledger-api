//go:build manualbcp

package bcp

import (
	"context"
	"testing"
)

// TestRealBCP_ConditionalRoundTrip exercises conditional requests
// against the live API. Build-tagged off: CI has no BCP data and must
// never call BCP (see CLAUDE.md). Run by hand when changing this path:
//
//	go test -tags=manualbcp ./internal/bcp/ -run TestRealBCP -v
//
// It fetches one already-concluded event twice. The first call stores
// BCP's weak ETag (W/"...") and its body; the second should revalidate
// and come back 304, which this can only observe indirectly — by the
// second result matching the first while the stored validator is in
// play.
func TestRealBCP_ConditionalRoundTrip(t *testing.T) {
	const endedEvent = "u1FYtThZ159d"

	c := NewClient()
	first, err := c.fetchEventInfoUncached(context.Background(), endedEvent)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	url := c.apiBaseV2 + "/events/" + endedEvent + "?role=true"
	if _, stored := c.conditional.body(url); !stored {
		t.Fatal("no response stored after the first fetch — BCP sent no ETag, or it wasn't kept")
	}

	second, err := c.fetchEventInfoUncached(context.Background(), endedEvent)
	if err != nil {
		t.Fatalf("second fetch (the conditional one): %v", err)
	}
	if second.ID != first.ID || second.Name != first.Name {
		t.Errorf("revalidated response differs: %q/%q vs %q/%q",
			second.ID, second.Name, first.ID, first.Name)
	}
	t.Logf("revalidated %q against a stored weak validator", second.Name)
}
