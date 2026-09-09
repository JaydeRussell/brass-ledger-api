package api

import (
	"context"
	"time"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
)

// bcpClient is the *bcp.Client surface this package's handlers actually
// call. An interface here — rather than the concrete *bcp.Client type —
// mirrors userStore (see auth.go): it's the seam that lets a handler's
// tests fake BCP responses directly instead of always standing up an
// httptest.Server, the same way userStore lets auth_test.go's
// fakeUserStore avoid a real database. *bcp.Client satisfies this
// automatically; no change needed in internal/bcp.
type bcpClient interface {
	FetchEventInfo(ctx context.Context, eventID string) (bcp.EventInfo, error)
	FetchPlayers(ctx context.Context, eventID string) ([]bcp.Player, error)
	FetchRoundPairings(ctx context.Context, eventID, pairingType string, round int) ([]bcp.PairingRecord, error)
	FetchPlacings(ctx context.Context, eventID string, teamEvent bool) ([]bcp.PlacingEntry, error)
	FetchCurrentItcLeagueIDForEvent(ctx context.Context, leagueIDs []string) (string, error)
	FetchItcRanking(ctx context.Context, leagueID, bcpUserID string) (*bcp.ItcRanking, error)
	FetchLeagueInfo(ctx context.Context, leagueID string) (*bcp.LeagueInfo, error)
	FetchPlayerEventHistory(ctx context.Context, bcpUserID string) ([]bcp.PlayerEventRecord, error)
	FetchPlacingHistory(ctx context.Context, bcpUserID string) ([]bcp.PlacingHistoryEntry, error)
	InvalidatePlayerEventHistory(bcpUserID string)
	InvalidateEventInfo(eventID string)
	PlayerEventHistoryFetchedAt(bcpUserID string) (time.Time, bool)
}
