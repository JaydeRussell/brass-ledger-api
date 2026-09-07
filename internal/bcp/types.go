// Package bcp is this service's client for Best Coast Pairings' (BCP)
// undocumented data API — moved here from the frontend so every browser
// stops hitting BCP independently and instead shares one server-side
// cache, with BCP's respectful-use rate limiting enforced once, in one
// place, for every user of this app rather than per browser tab.
//
// SCOPE NOTE (carried over from the frontend's original lib/bcp.ts):
// this package only ever retrieves data BCP already publishes (rosters,
// event metadata, already-decided pairings, already-computed placings).
// It never computes, ranks, or suggests a pairing/matchup of any kind —
// Challengers Cup's event pack bans "AI programs, algorithms, or
// methodology... for the pairings process," which is broader than just
// AI. Don't add scoring or suggestion logic here, even without any AI
// involved.
package bcp

// EventInfo is metadata about a BCP event — its display name, whether
// it's a team event, how many rounds are underway/published, and the
// event-facts BCP's own Overview tab shows. All of this is metadata BCP
// already published; nothing here is computed or inferred.
type EventInfo struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	TeamEvent         bool     `json:"teamEvent"`
	Started           bool     `json:"started"`
	Ended             bool     `json:"ended"`
	CurrentRound      int      `json:"currentRound"`
	NumberOfRounds    int      `json:"numberOfRounds"`
	Description       string   `json:"description,omitempty"`
	GameSystem        string   `json:"gameSystem,omitempty"`
	GameSystemID      string   `json:"gameSystemId,omitempty"`
	StartDate         string   `json:"startDate,omitempty"`
	EndDate           string   `json:"endDate,omitempty"`
	Location          string   `json:"location,omitempty"`
	Organizer         string   `json:"organizer,omitempty"`
	RegistrationLabel string   `json:"registrationLabel,omitempty"`
	RegistrationCount string   `json:"registrationCount,omitempty"`
	PlayerCount       *int     `json:"playerCount,omitempty"`
	Circuits          []string `json:"circuits,omitempty"`
}

// Player is one registered player's roster entry — deliberately minimal
// (no matchup score, no pairing state), matching the frontend's own
// scope limit: this app only displays public roster data, never ranks
// or suggests pairings from it.
type Player struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Faction      string `json:"faction"`
	SubFaction   string `json:"subFaction,omitempty"`
	Team         string `json:"team,omitempty"`
	TeamPlayerID string `json:"teamPlayerId,omitempty"`
	HomeClub     string `json:"homeClub,omitempty"`
	List         string `json:"list,omitempty"`
	BcpUserID    string `json:"bcpUserId,omitempty"`
}

// PairingUserRef is BCP's global (cross-event) account reference nested
// on a pairing's player.
type PairingUserRef struct {
	ID        string `json:"id,omitempty"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
}

// PairingPlayerRef is one side of an individual ("Pairing"-type) matchup.
type PairingPlayerRef struct {
	ID   string          `json:"id"`
	User *PairingUserRef `json:"user,omitempty"`
}

// PairingTeamRef is one side of a team-vs-team ("TeamPairing"-type)
// matchup.
type PairingTeamRef struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	CaptainID string `json:"captainId,omitempty"`
}

// GameResult is one side's already-published game score. `Result` is
// some internal BCP enum code whose exact meaning isn't documented —
// same as the frontend's original note, callers compare `Points`
// directly (whoever has more won) rather than trying to interpret it.
type GameResult struct {
	ID     string   `json:"id,omitempty"`
	Result *int     `json:"result,omitempty"`
	Points *float64 `json:"points,omitempty"`
}

// PairingRecord is one pairing exactly as BCP's API returns it — passed
// through mostly as-is (same field names BCP itself uses) rather than
// reshaped, so the frontend's existing "which side is mine"/"which side
// goes first" display logic keeps working unchanged against this
// service instead of against BCP directly.
type PairingRecord struct {
	ID            string            `json:"id"`
	PairingType   string            `json:"pairingType"`
	Table         *int              `json:"table,omitempty"`
	Round         *int              `json:"round,omitempty"`
	Published     bool              `json:"published,omitempty"`
	IsDone        bool              `json:"isDone,omitempty"`
	Player1ID     string            `json:"player1Id,omitempty"`
	Player2ID     string            `json:"player2Id,omitempty"`
	Player1       *PairingPlayerRef `json:"player1,omitempty"`
	Player2       *PairingPlayerRef `json:"player2,omitempty"`
	TeamPairingID string            `json:"teamPairingId,omitempty"`
	TeamPlayer1   *PairingTeamRef   `json:"teamPlayer1,omitempty"`
	TeamPlayer2   *PairingTeamRef   `json:"teamPlayer2,omitempty"`
	Player1Game   *GameResult       `json:"player1Game,omitempty"`
	Player2Game   *GameResult       `json:"player2Game,omitempty"`
}

// PlacingEntry is one row of the event's standings — whatever BCP has
// already computed and published. Metrics are left as whatever named
// values BCP reports (e.g. "Wins", "Battle Points") rather than
// hardcoded fields, since these vary by event/scoring format.
type PlacingEntry struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Placing *int            `json:"placing,omitempty"`
	Metrics []PlacingMetric `json:"metrics"`
}

type PlacingMetric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// ItcRanking is one player's current ITC ranking within a league: their
// total points and overall rank, exactly as BCP has already computed
// and published it.
type ItcRanking struct {
	Points  float64 `json:"points"`
	Placing *int    `json:"placing,omitempty"`
	Wins    *int    `json:"wins,omitempty"`
	Losses  *int    `json:"losses,omitempty"`
	Ties    *int    `json:"ties,omitempty"`
}

// PlayerEventRecord is one event a BCP user has ever submitted a roster
// for — past, in progress, or upcoming alike. Deliberately minimal:
// "which events", not a full per-event roster entry (see Player for
// that). This alone doesn't say whether an event is past/present/future
// — BCP's own response here only expands the event to {id, name}, no
// dates — so classifying it needs a separate FetchEventInfo call (or,
// for already-concluded events, PlacingHistoryEntry below already has
// the dates inline).
type PlayerEventRecord struct {
	EventID   string `json:"eventId"`
	EventName string `json:"eventName"`
	CheckedIn bool   `json:"checkedIn"`
	Dropped   bool   `json:"dropped"`
}

// PlacingHistoryEntry is one already-concluded event's final result for
// a BCP user: their placing, points, and what they played, plus the
// event's own dates — BCP's placings-history endpoint expands these
// inline, unlike the plain registration endpoint PlayerEventRecord comes
// from.
type PlacingHistoryEntry struct {
	EventID      string   `json:"eventId"`
	EventName    string   `json:"eventName"`
	EventDate    string   `json:"eventDate,omitempty"`
	EventEndDate string   `json:"eventEndDate,omitempty"`
	Placing      *int     `json:"placing,omitempty"`
	Points       *float64 `json:"points,omitempty"`
	Faction      string   `json:"faction,omitempty"`
	Team         string   `json:"team,omitempty"`
}
