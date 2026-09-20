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
	RoundTripCost = 300 * time.Millisecond

	// DurableReadCost is one read of the durable cache — a single query
	// against Neon from the container.
	//
	// Measured 2026-09-20 from the clearest natural experiment this
	// project has had: /api/me/stats took 2,880ms while resolving 41
	// events through the durable cache one at a time (the prewarm had
	// silently stopped batching). 2,880 / 41 ≈ 70ms.
	DurableReadCost = 70 * time.Millisecond

	// OwnOverheadCost is everything this service does itself for one
	// request: routing, the session lookup, serialising the response.
	//
	// Measured from the access log's own latency_human on requests that
	// do no upstream work at all — 573µs to 790µs. Rounded up; it is
	// three orders of magnitude below the others and is included only so
	// the estimate doesn't read as exactly zero for a request that
	// touches nothing.
	OwnOverheadCost = time.Millisecond
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
// CALIBRATION, against four real production measurements from
// 2026-09-20 (re-run these whenever the constants change):
//
//	case                                       estimate  actual  ratio
//	v0.19.2 /api/me/stats, 41 serial reads       2.871s  2.880s   1.00
//	v0.19.3 /api/me/events, 9 serial BCP calls   2.911s  2.942s   0.99
//	v0.19.4 /api/me/events warm, 3 at a time       511ms   723ms   0.71
//	v0.19.4 /api/me/stats warm, batched            211ms   279ms   0.76
//
// So it is near-exact on the slow cases — the ones worth catching — and
// runs roughly 25% optimistic when everything is already fast, because
// it models only round trips and ignores TLS, the Worker-to-container
// hop, and serialising a larger response.
//
// That makes it a LOWER BOUND, which matters when choosing a budget to
// assert against: leave headroom below the real threshold rather than
// asserting at it, or a 1.9s estimate could be a 2.4s page. See
// estimatedPageBudget in internal/api/latency_budget_test.go.
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
