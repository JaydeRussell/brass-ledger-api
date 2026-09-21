package bcp

import "time"

// Observed costs of the things a request waits on in production.
//
// These exist so a test that never leaves the machine can still say
// something about seconds. Counting round trips is deterministic and
// offline; multiplying those counts by what a round trip actually costs
// turns them into an estimate in the same units as the rule they're
// defending ("a page over two seconds is a bug" — see CLAUDE.md). A
// structural regression then fails as "this would take 3.1s in
// production", not as "the count went from 3 to 12", which is a much
// easier thing to act on.
//
// They are averages of real production measurements, not guesses, and
// they are only as good as their last calibration. Re-measure with
// `scripts/latency-map.sh --base https://api.brass-ledger.app` (with a
// session for a linked account) and update these when the deployment
// changes shape — a different region, a different database plan, BCP
// getting faster or slower.
//
// Deliberately rounded. The point is to catch "this became ten sequential
// round trips", which is an order-of-magnitude question; precision past
// the nearest few tens of milliseconds would be false confidence.
const (
	// RoundTripCost is one call to BCP's API from the deployed
	// container.
	//
	// Measured 2026-09-20: /api/me/events spent 2,942ms resolving 9
	// registrations that had no published placing, one per event and in
	// series — ~327ms each. A rejected request (an unknown event id)
	// comes back nearer 190ms, so this is deliberately the cost of a
	// real one carrying real data.
	//
	// NOT re-measured on 2026-09-21, deliberately, while the other two
	// were. Isolating one BCP round trip from outside needs either an
	// event that is live (so nothing is durably cached) or a stream of
	// deliberately doomed requests for ids that don't exist — and the
	// second is the exact pattern CLAUDE.md's respect rule is about. A
	// constant from a real production observation is worth more than one
	// from a worse measurement, so this keeps the 2026-09-20 figure.
	// Measuring it properly wants instrumentation on the inside, timing
	// the BCP calls real traffic already makes, rather than another
	// round of synthetic requests from the outside.
	RoundTripCost = 300 * time.Millisecond

	// DurableReadCost is one read of the durable cache — a single query
	// against Neon from the container.
	//
	// Re-measured 2026-09-21 against production with a real session:
	// /readyz, which pings the database, ran 228ms p50 against
	// /healthz's 148ms, which doesn't. The difference is one round trip
	// to Neon.
	//
	// The previous 70ms came from a different and cruder experiment
	// (/api/me/stats resolving 41 events one at a time, 2,880/41), which
	// is close enough to corroborate rather than contradict this.
	DurableReadCost = 80 * time.Millisecond

	// OwnOverheadCost is what one request costs before it does any
	// upstream work at all: reaching this service and answering.
	//
	// 150ms, re-measured 2026-09-21: /healthz, which touches neither the
	// database nor BCP, ran 148ms p50 in production. That is the floor
	// for any request, and it is almost entirely the hop in front of the
	// container rather than anything the Go process does.
	//
	// It was 1ms until now, and the old figure was not a typo — it was
	// measured correctly from the wrong place. The access log's
	// latency_human is what the container spends between receiving a
	// request and answering it: 573µs to 790µs, quite true. What it
	// cannot see is everything before that. The Worker forwards to a
	// Durable Object which forwards to the container, and that hop is
	// roughly 145ms of the 148.
	//
	// The practical effect of the error: every estimate this model
	// produced was low by a flat ~149ms per request. That never hid a
	// structural regression — a round-trip count going from 3 to 12 is
	// just as visible either way — but it did mean the "this would take
	// 3.1s in production" claim was systematically optimistic by more
	// than the 25% the calibration table blamed on modelling only round
	// trips. Corroborated by /api/me/stats warm: predicted ~71ms,
	// measured 227ms, and the gap is this floor almost exactly.
	OwnOverheadCost = 150 * time.Millisecond
)

// EstimatedCost models what a request costs in production, given what a
// test observed it doing.
//
// The model is deliberately crude, and its one real insight is that
// *serialisation is what costs*: n calls made one after another cost n
// round trips, while the same n made c at a time cost ceil(n/c). Every
// latency bug this project has had was a change in that ratio.
//
// upstreamCalls and maxConcurrent come from counting an httptest stub —
// see internal/api/latency_budget_test.go. durableReads is per-key
// reads, durableBatches is batched ones; a batch is charged as a single
// read, which is the entire point of batching.
//
// This is an estimate, not a measurement. Production remains the source
// of truth.
//
// CALIBRATION.
//
// Against production on 2026-09-21, with the constants above:
//
//	case                                  estimate  actual  ratio
//	/healthz (nothing but the floor)         150ms   148ms   1.01
//	/api/me/stats warm, one batched read     230ms   227ms   1.01
//
// Against production on 2026-09-20, with OwnOverheadCost still at 1ms
// — kept because the slow rows are still the best evidence for
// RoundTripCost, which has not been re-measured since:
//
//	case                                       estimate  actual  ratio
//	v0.19.2 /api/me/stats, 41 serial reads       2.871s  2.880s   1.00
//	v0.19.3 /api/me/events, 9 serial BCP calls   2.911s  2.942s   0.99
//	v0.19.4 /api/me/events warm, 3 at a time       511ms   723ms   0.71
//	v0.19.4 /api/me/stats warm, batched            211ms   279ms   0.76
//	v0.19.6 /api/me/events warm, 2 dead lookups    371ms   731ms   0.51
//
// The 2026-09-20 rows explain themselves once OwnOverheadCost is right.
// The slow cases came out near-exact because a missing 149ms is noise
// against three seconds; the fast ones looked "25% optimistic" because
// 149ms is most of a 500ms request. It was not a modelling subtlety
// about TLS and response size, as this comment used to claim — it was
// one constant that measured the container rather than the trip to it.
// The two rows above, taken after fixing it, sit at 1.01.
//
// The last row is the model working as intended and being read wrong by
// me rather than being wrong: I predicted /api/me/events at ~222ms warm
// and measured 731ms, because the count I fed it was the count I
// believed, not the count the endpoint made. Two registrations pointed
// at deleted events, whose 404s were never cached, so every request
// paid a round trip the model never heard about. The estimate is only
// as honest as the observation behind it — which is why the round-trip
// counts now come from a stub that counts (latency_budget_test.go)
// rather than from reading the code and reasoning.
//
// It is still a LOWER BOUND, and still worth leaving headroom below the
// real threshold rather than asserting at it — but for a smaller reason
// than before. What it now omits is TLS setup, serialising a large
// response, and a cold container start, not a flat 149ms on every
// request. See estimatedPageBudget in internal/api/latency_budget_test.go.
func EstimatedCost(upstreamCalls, maxConcurrent, durableReads, durableBatches int) time.Duration {
	concurrency := maxConcurrent
	if concurrency < 1 {
		concurrency = 1
	}

	// Round up: 9 calls three at a time is three rounds, not two.
	rounds := (upstreamCalls + concurrency - 1) / concurrency

	return time.Duration(rounds)*RoundTripCost +
		time.Duration(durableReads+durableBatches)*DurableReadCost +
		OwnOverheadCost
}
