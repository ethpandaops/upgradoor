package userapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
)

// Window of the slot history, in seconds, and how many points an answer
// carries at most. A window with more slots than that is served in buckets.
const (
	defaultSlotWindow = 3 * 3600
	minSlotWindow     = 60
	maxSlotWindow     = 12 * 3600
	maxSlotPoints     = 400
)

// SlotHistory is the body of GET /api/v1/public/slots and GET /api/v1/slots:
// what the tracked validators did slot by slot over a window that ends now.
//
// The answer is columnar. Point i of every column covers the BucketSlots
// slots from FirstSlot + i*BucketSlots on; the last point is the bucket the
// clock is in. With BucketSlots 1 a point is one slot. A longer window is
// served in buckets of a power of two slots, aligned to the slot number, so a
// bucket holds the same slots in every answer.
type SlotHistory struct {
	// FirstSlot is the first slot of point 0 and FirstTime when it began.
	// SlotMs is the slot duration there; a client places the points by it.
	FirstSlot     uint64    `json:"firstSlot"`
	FirstTime     time.Time `json:"firstTime"`
	SlotMs        uint64    `json:"slotMs"`
	SlotsPerEpoch uint64    `json:"slotsPerEpoch"`
	BucketSlots   uint64    `json:"bucketSlots"`

	// MinParticipationPercent is the threshold the network gate holds the
	// per-epoch participation to; EpochSeconds the length of an epoch.
	MinParticipationPercent float64 `json:"minParticipationPercent"`
	EpochSeconds            float64 `json:"epochSeconds"`

	// Recorded counts the slots of a point that are stored: 0 is a point
	// nothing is known about (not settled yet, or the controller was not
	// watching). Missed counts the recorded slots without a block.
	Recorded []uint64 `json:"recorded"`
	Missed   []uint64 `json:"missed"`
	// Assigned and Included sum the tracked validators with an attestation
	// duty and the ones whose attestation a block carries.
	Assigned []uint64 `json:"assigned"`
	Included []uint64 `json:"included"`
	// WorstAssigned and WorstIncluded are the counts of the slot of a point
	// with the lowest share included, so a dip stays visible in a bucket.
	// They are left out when a point is one slot.
	WorstAssigned []uint64 `json:"worstAssigned,omitempty"`
	WorstIncluded []uint64 `json:"worstIncluded,omitempty"`

	// Outages are the spans of the window during which hosts were off:
	// because upgradoor worked on them (a lease), or without it (they missed
	// attestations in OffSlots slots or more, no further than an epoch
	// apart). The list is bounded: with many, spans of one kind that overlap
	// stand as one span for several hosts. OutageHosts is how many different
	// hosts were off in the window, whatever the list was merged to.
	//
	// Only the authenticated route carries them: which host was off is not
	// public. There is no row per host and slot: a fleet of a thousand hosts
	// that attests is an empty list.
	Outages     []Outage `json:"outages,omitempty"`
	OutageHosts int      `json:"outageHosts,omitempty"`
	OffSlots    uint64   `json:"offSlots,omitempty"`
}

// handlePublicSlots serves GET /api/v1/public/slots?window=<seconds>: the
// fleet-wide participation and the missed proposals per slot. That is chain
// health; the split per host is behind the login (handleSlots).
func (s *Handler) handlePublicSlots(w http.ResponseWriter, r *http.Request) {
	s.serveSlots(w, r, false)
}

// handleSlots serves GET /api/v1/slots?window=<seconds>: the public slot
// history with the split per host.
func (s *Handler) handleSlots(w http.ResponseWriter, r *http.Request, _ *authz.User) {
	s.serveSlots(w, r, true)
}

func (s *Handler) serveSlots(w http.ResponseWriter, r *http.Request, perHost bool) {
	ctx := r.Context()

	window, err := queryWindow(r)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	health := s.opts.Beacon.Health()
	now := s.now()

	out := SlotHistory{
		SlotsPerEpoch:           health.SlotsPerEpoch,
		BucketSlots:             1,
		MinParticipationPercent: settings.MinParticipationPercent,
		EpochSeconds:            health.EpochDuration().Seconds(),
		Recorded:                []uint64{},
		Missed:                  []uint64{},
		Assigned:                []uint64{},
		Included:                []uint64{},
	}

	span, ok := slotSpan(&health, now, window)
	if !ok {
		// The chain's timing is not known (yet): there is nothing to place.
		httpjson.Write(s.log, w, http.StatusOK, out)

		return
	}

	rows, err := s.opts.Store.ListSlotParticipation(ctx, span.first)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	span.fill(&out, rows)

	if perHost {
		if err := s.slotOutages(ctx, &out, &span, &health, now, window); err != nil {
			httpjson.InternalError(s.log, w, r, err)

			return
		}
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}

// queryWindow parses the "window" query parameter: seconds, at most
// maxSlotWindow.
func queryWindow(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("window")
	if raw == "" {
		return defaultSlotWindow * time.Second, nil
	}

	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < minSlotWindow {
		return 0, httpjson.BadRequestf("window must be a number of seconds, at least %d", minSlotWindow)
	}

	return time.Duration(min(seconds, maxSlotWindow)) * time.Second, nil
}

// span is the slots a slot history covers and how they are put into points.
type span struct {
	// first is the first slot of point 0, last the slot the clock is in.
	first, last uint64
	// bucket is the number of slots per point.
	bucket uint64
	// start is when first began and slotMs the slot duration there.
	start  time.Time
	slotMs uint64
}

// slotSpan places a window that ends at now. ok is false when the chain's
// timing does not place now.
func slotSpan(health *fleet.NetworkHealth, now time.Time, window time.Duration) (span, bool) {
	last, ok := health.SlotAt(now)
	if !ok {
		return span{}, false
	}

	// A window that reaches back before the chain begins at its first slot.
	var from uint64

	if begin, placed := health.TimingStart(); placed && now.Add(-window).After(begin) {
		if from, ok = health.SlotAt(now.Add(-window)); !ok {
			return span{}, false
		}
	}

	out := span{last: last, bucket: 1}
	for (last-from)/out.bucket >= maxSlotPoints {
		out.bucket *= 2
	}

	out.first = from - from%out.bucket

	if out.start, ok = health.SlotStart(out.first); !ok {
		return span{}, false
	}

	length, ok := health.SlotDuration(out.first / health.SlotsPerEpoch)
	if !ok {
		return span{}, false
	}

	out.slotMs = uint64(length / time.Millisecond)

	return out, true
}

// points is the number of points of the span.
func (p *span) points() int {
	return int((p.last-p.first)/p.bucket) + 1
}

// fill puts the slot rows into the columns of out.
func (p *span) fill(out *SlotHistory, rows []fleet.SlotParticipation) {
	points := p.points()

	out.FirstSlot = p.first
	out.FirstTime = p.start.UTC()
	out.SlotMs = p.slotMs
	out.BucketSlots = p.bucket
	out.Recorded = make([]uint64, points)
	out.Missed = make([]uint64, points)
	out.Assigned = make([]uint64, points)
	out.Included = make([]uint64, points)

	if p.bucket > 1 {
		out.WorstAssigned = make([]uint64, points)
		out.WorstIncluded = make([]uint64, points)
	}

	for i := range rows {
		row := &rows[i]
		if row.Slot < p.first || row.Slot > p.last {
			continue
		}

		point := (row.Slot - p.first) / p.bucket

		out.Recorded[point]++
		out.Assigned[point] += row.Assigned
		out.Included[point] += row.Included

		if !row.Proposed {
			out.Missed[point]++
		}

		if p.bucket == 1 || row.Assigned == 0 {
			continue
		}

		// The lowest share so far: included/assigned compared without
		// dividing. The first slot with a duty starts it.
		worstAssigned, worstIncluded := out.WorstAssigned[point], out.WorstIncluded[point]
		if worstAssigned == 0 || row.Included*worstAssigned < worstIncluded*row.Assigned {
			out.WorstAssigned[point], out.WorstIncluded[point] = row.Assigned, row.Included
		}
	}
}

// slotOutages adds the outages of the window to out.
func (s *Handler) slotOutages(
	ctx context.Context, out *SlotHistory, p *span, health *fleet.NetworkHealth,
	now time.Time, window time.Duration,
) error {
	// Newest first: enough of them to cover the window.
	leases, err := s.opts.Store.ListLeases(ctx, store.LeaseFilter{Limit: maxOutageLeases})
	if err != nil {
		return err
	}

	// The runs of missed slots are found in the database; only the hosts
	// with a lease in the window come slot by slot, for the lease to explain
	// its own slots.
	windowStart, _ := health.SlotStart(p.first)

	runs, err := s.opts.Store.SumHostSlotRuns(ctx, p.first, p.last, max(health.SlotsPerEpoch, 1),
		outageMinSlots, leaseHosts(leases, windowStart.UTC(), now))
	if err != nil {
		return err
	}

	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return err
	}

	validators := make(map[string]uint64, len(hosts))
	for _, host := range hosts {
		validators[host.Name] = host.Weight()
	}

	spans := outageSpans(&outageInput{
		health: health, first: p.first, last: p.last, now: now,
		runs: runs, leases: leases, validators: validators,
	})

	off := make(map[string]struct{}, len(spans))
	for i := range spans {
		off[spans[i].Names[0]] = struct{}{}
	}

	out.OutageHosts = len(off)
	out.OffSlots = outageMinSlots
	out.Outages = capOutages(spans, maxOutages, window)

	return nil
}
