package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/JaydeRussell/brass-ledger-api/internal/bcp"
	"github.com/JaydeRussell/brass-ledger-api/internal/timing"
)

// GET /api/admin/costs — what the cost model assumes, next to what this
// process has actually observed.
//
// internal/bcp/cost.go's constants are how a round-trip count becomes a
// claim in seconds, and they are only as good as their last
// calibration. One of them sat wrong by two orders of magnitude for
// months (OwnOverheadCost, 1ms against a real 150ms) because it was
// measured from the access log, which cannot see the hop in front of
// the container. Nothing surfaced the drift; it took holding an outside
// measurement against an inside one to notice.
//
// So this reports both numbers side by side. Drift becomes something
// you can see at a glance rather than something you find months later.
//
// It never measures anything on demand. Every figure here is timed from
// work the service was going to do anyway — the moment an endpoint can
// trigger a call to BCP to find out how long one takes, it is sending
// traffic BCP did not ask for, which is the rule this whole service is
// built around.

// costObservation is one modelled cost and what was actually seen.
type costObservation struct {
	// AssumedMs is what internal/bcp/cost.go currently models.
	AssumedMs int64 `json:"assumedMs"`
	// ObservedP50Ms and ObservedP90Ms are omitted entirely when nothing
	// has been sampled, rather than reported as zero — a zero here would
	// read as "instant", which is the opposite of "unknown".
	ObservedP50Ms *int64 `json:"observedP50Ms,omitempty"`
	ObservedP90Ms *int64 `json:"observedP90Ms,omitempty"`
	// SampleCount is what the percentiles are drawn from. Read a
	// percentile next to a small count with the suspicion it deserves:
	// the container sleeps after ten idle minutes and takes these with
	// it, so this is a recent window, never a lifetime.
	SampleCount int64 `json:"sampleCount"`
	// Note explains anything the numbers alone would mislead about.
	Note string `json:"note,omitempty"`
}

type costsResponse struct {
	// ProcessUptimeSeconds frames everything else: percentiles from a
	// process that started a minute ago describe a minute.
	ProcessUptimeSeconds int64 `json:"processUptimeSeconds"`

	BcpFetch      costObservation `json:"bcpFetch"`
	BcpRevalidate costObservation `json:"bcpRevalidate"`
	DurableRead   costObservation `json:"durableRead"`
	OwnOverhead   costObservation `json:"ownOverhead"`
}

func observed(assumed time.Duration, snap timing.Snapshot, note string) costObservation {
	out := costObservation{
		AssumedMs:   assumed.Milliseconds(),
		SampleCount: snap.Count,
		Note:        note,
	}
	if snap.Count > 0 {
		p50, p90 := snap.P50.Milliseconds(), snap.P90.Milliseconds()
		out.ObservedP50Ms, out.ObservedP90Ms = &p50, &p90
	}
	return out
}

// durableReader is the part of internal/bcpcache this needs — declared
// here rather than imported concretely so the handler stays testable
// against a fake, the same way every other handler in this package
// treats its stores.
type durableReader interface {
	ReadTimings() timing.Snapshot
}

// CostsHandler serves the observed-costs report.
type CostsHandler struct {
	store   userStore
	client  *bcp.Client
	durable durableReader
	started time.Time
}

// NewCostsHandler builds a CostsHandler. durable may be nil when no
// durable cache is configured, which is the local-development default.
func NewCostsHandler(store userStore, client *bcp.Client, durable durableReader) *CostsHandler {
	return &CostsHandler{store: store, client: client, durable: durable, started: time.Now()}
}

// Register wires this handler's route onto e.
func (h *CostsHandler) Register(e *echo.Echo) {
	e.GET("/api/admin/costs", h.Costs)
}

// Costs is GET /api/admin/costs. Admin-only: it is operational detail
// about the deployment, and it is nobody's business but an operator's.
// It reports timings and counts and nothing else — never what was
// fetched, or for whom.
func (h *CostsHandler) Costs(c echo.Context) error {
	if _, err := requireAdmin(c, h.store); err != nil {
		return err
	}
	noCache(c)

	fetches, revalidations := h.client.Timings()

	var durableSnap timing.Snapshot
	if h.durable != nil {
		durableSnap = h.durable.ReadTimings()
	}

	return c.JSON(http.StatusOK, costsResponse{
		ProcessUptimeSeconds: int64(time.Since(h.started).Seconds()),
		BcpFetch: observed(bcp.RoundTripCost, fetches,
			"A real response carrying data. This is what RoundTripCost models."),
		BcpRevalidate: observed(bcp.RoundTripCost, revalidations,
			"A 304 answering If-None-Match. Counted separately: these are much cheaper, and "+
				"averaging them into the line above would make a real fetch look faster than it is."),
		DurableRead: observed(bcp.DurableReadCost, durableSnap,
			"One read of the durable cache. Writes are excluded — they no longer sit on a request."),
		OwnOverhead: observed(bcp.OwnOverheadCost, timing.Snapshot{},
			"Not observable from in here, and that is the point: this process cannot see the hop "+
				"in front of it, which is most of the cost. Measure it from outside with /healthz, "+
				"which touches neither the database nor BCP."),
	})
}
