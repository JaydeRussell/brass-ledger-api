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
//
// Why "matchup" is named separately from "pairing" here, and why this
// rule is deliberately broader than the event pack's literal "pairings
// process" wording (settled 2026-09-12, after a "favored team" indicator
// was proposed, discussed, and declined — see brass-ledger-web's
// ROADMAP.md "Declined" section for the full writeup): team events don't
// actually finish "pairing" the moment BCP publishes a team-vs-team
// matchup. Captains then run a live "Defender/Attacker" process to
// assign *individual boards* within that already-decided team pairing —
// one team names a Defender, the other names two Attackers, the
// Defender's captain picks one — using "team goals, list roles, and
// expected scoring outcomes" as real input (confirmed via live research,
// not assumed). A computed comparison between two sides — even one
// scoped to the whole team rather than any single board, even a plain
// average-ITC comparison with no AI involved — could function as
// decision support for that still-active, human-driven step. That's
// what this rule actually protects against: not just literal pairing
// generation, but anything that could feed a pairing-adjacent decision
// someone is still in the middle of making.
//
// What stays fine: plainly displaying two already-published numbers
// next to each other — e.g. both sides' already-published average ITC
// shown side by side on a team pairing — with no framing, ranking,
// "favored"/"underdog" label, or color-coding tied to which one is
// higher. That's not a new computation, just data already available
// elsewhere (a team's own roster) shown in one more place. The line is
// *computing or presenting a comparative judgment* about the matchup,
// not *displaying two independent facts* that happen to sit next to
// each other.
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
	// League ids this event is scored under (BCP's own "leagues" array on
	// the event) — backend-internal only (json:"-"), used to resolve the
	// current flagship ITC league anchored on this specific event rather
	// than searching BCP's full game-system-wide leagues list, which no
	// longer reliably surfaces it (see itc.go's
	// FetchCurrentItcLeagueIDForEvent doc comment for the full story).
	LeagueIDs []string `json:"-"`
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
	Disposition  string `json:"disposition,omitempty"`
	Team         string `json:"team,omitempty"`
	TeamPlayerID string `json:"teamPlayerId,omitempty"`
	HomeClub     string `json:"homeClub,omitempty"`
	List         string `json:"list,omitempty"`
	BcpUserID    string `json:"bcpUserId,omitempty"`
}

// forceDispositions are 40k 11th edition's five Force Dispositions — the
// mission-pack-assigned role a submitted army list's detachment(s)
// represent (Take and Hold, Purge the Foe, Disruption, Reconnaissance,
// Priority Assets). BCP has no dedicated field for this: for events
// using this mission system, it reuses the same `subFaction` slot a
// real army sub-faction (chapter, craftworld, etc.) would otherwise
// occupy — confirmed live against a real event where every one of 368
// players' subFaction values was exactly one of these five strings.
// Player.Disposition is populated only when SubFaction is an exact
// match, so an event that isn't using Force Disposition (where
// SubFaction holds a genuine sub-faction name) is unaffected.
var forceDispositions = map[string]bool{
	"Take and Hold":   true,
	"Purge the Foe":   true,
	"Disruption":      true,
	"Reconnaissance":  true,
	"Priority Assets": true,
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
//
// BcpUserID is only ever set for an individual-event row (BCP's own
// standings resource carries a `user` object there the same way
// FetchPlayers' roster resource does — see bcpPlacingRecord.User); a
// team-event row's `name` is the team itself, not one person, so there's
// no single BCP account to point it at and this stays empty.
type PlacingEntry struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Placing   *int            `json:"placing,omitempty"`
	Metrics   []PlacingMetric `json:"metrics"`
	BcpUserID string          `json:"bcpUserId,omitempty"`
}

// PlacingMetric is one named scoring value BCP reports for a placing
// (e.g. "Wins", "Battle Points") — see PlacingEntry's doc comment for
// why these vary by event/scoring format instead of being fixed fields.
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
//
// LeagueID matters more than it looks: BCP scores one event under
// several leagues/circuits at once (its flagship ITC ranking, a
// separate "Hobby Track" scoring, sometimes an old legacy default league
// too), so a single event can appear here as *multiple* entries sharing
// an EventID but with different LeagueID/Placing/Points — see
// FetchLeagueInfo and internal/api/stats.go's canonicalPlacingPerEvent
// for how a caller picks the one that actually counts as "the" placing.
type PlacingHistoryEntry struct {
	EventID      string   `json:"eventId"`
	EventName    string   `json:"eventName"`
	EventDate    string   `json:"eventDate,omitempty"`
	EventEndDate string   `json:"eventEndDate,omitempty"`
	Placing      *int     `json:"placing,omitempty"`
	Points       *float64 `json:"points,omitempty"`
	Faction      string   `json:"faction,omitempty"`
	Team         string   `json:"team,omitempty"`
	LeagueID     string   `json:"leagueId,omitempty"`
}

// LeagueInfo is what a caller needs to know about one of BCP's
// leagues/circuits to decide whether a placing scored under it is "the"
// competitive placing for an event, as opposed to a parallel Hobby
// Track score or an old legacy default league — see FetchLeagueInfo.
type LeagueInfo struct {
	Name  string `json:"name"`
	GwItc bool   `json:"gwItc"`
	Hobby bool   `json:"hobby"`
}
