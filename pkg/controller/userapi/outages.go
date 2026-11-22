package userapi

import (
	"cmp"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The outages of a slot history: the spans during which hosts were off, as
// the participation graph draws them. They are worked out here, not in the
// page, from two things that do not grow with a healthy fleet: the leases of
// the window, and the slots in which a host missed attestations (only those
// are stored per host). The answer is a list of spans, bounded however large
// the fleet is.

const (
	// outageMinSlots is how many slots with missed attestations make a host
	// "off" when no lease explains them: a miss or two is not an outage (it
	// shows in the columns of the graph).
	outageMinSlots = 3
	// maxOutages bounds the spans of an answer. With more, spans of the same
	// kind that overlap are merged into one that stands for several hosts.
	maxOutages = 240
	// maxOutageNames bounds the host names a merged span carries; the count
	// says how many there are.
	maxOutageNames = 8
	// maxOutageLeases bounds the leases read for one answer.
	maxOutageLeases = 4000
)

// Kinds of work of a lease's span.
const (
	workMixed = "mixed"
)

// Outage is a span during which a host was off, or several hosts when the
// spans of a kind were merged.
type Outage struct {
	// views.Hosts is how many hosts the span stands for; Names are their names, at
	// most maxOutageNames of them, ordered.
	Hosts int      `json:"hosts"`
	Names []string `json:"names"`

	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// FromStart: the host was already off when the window began. Open: it is
	// still off (To is the last moment that is known of).
	FromStart bool `json:"fromStart,omitempty"`
	Open      bool `json:"open,omitempty"`

	// Lease is the lease that explains the span, when it stands for one
	// host: upgradoor was working on it. Work is what the lease does
	// ("image", "packages", "reboot", or "mixed"); it is empty for a span no
	// lease explains. Rollout is the rollout the lease belongs to (the first,
	// when it serves several). Failed: the lease failed or expired.
	Lease   string `json:"lease,omitempty"`
	Work    string `json:"work,omitempty"`
	Rollout string `json:"rollout,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
	// Deployed: the lease applies what a deploy on the host (upgradoor
	// deploy) set, for at least one of the hosts.
	Deployed bool `json:"deployed,omitempty"`

	// Missed and Assigned count the attestation duties in the slots of the
	// span in which the hosts missed some; Missed equal to Assigned means
	// nothing of them attested. Validators is how many validators the hosts
	// run.
	Missed     uint64 `json:"missed"`
	Assigned   uint64 `json:"assigned"`
	Validators uint64 `json:"validators"`
}

// outageInput is what the spans are made of.
type outageInput struct {
	health *fleet.NetworkHealth
	// first is the first slot of the window, last the slot the clock is in.
	first, last uint64
	now         time.Time
	// runs are the runs of slots in which a host missed attestations
	// (Store.SumHostSlotRuns). The hosts with a lease in the window
	// (leaseHosts) come slot by slot, so a lease explains its own slots.
	runs []fleet.HostSlotRun
	// leases are the leases that may overlap the window.
	leases []*fleet.Lease
	// validators is the number of validators per host.
	validators map[string]uint64
}

// leaseWork names what a lease does. An override or spec need recreates a
// container like an image need does, so it counts as an image roll.
func leaseWork(l *fleet.Lease) string {
	work := ""

	for i := range l.Needs {
		kind := string(l.Needs[i].Kind)

		switch l.Needs[i].Kind {
		case protocol.NeedOverride, protocol.NeedSpec:
			kind = string(protocol.NeedImage)
		default:
		}

		switch {
		case work == "":
			work = kind
		case work != kind:
			return workMixed
		}
	}

	if work == "" {
		// A lease without needs was adopted from an agent's report.
		return string(protocol.NeedImage)
	}

	return work
}

// leaseDeployed reports whether a lease applies a deploy on the host.
func leaseDeployed(l *fleet.Lease) bool {
	return slices.ContainsFunc(l.Needs, func(n protocol.Need) bool { return n.Kind == protocol.NeedSpec })
}

// leaseRollout is the rollout a lease belongs to: the smallest ID when it
// serves several, so the answer does not depend on the order of a map.
func leaseRollout(l *fleet.Lease) string {
	out := ""

	for _, id := range l.Rollouts {
		if out == "" || id < out {
			out = id
		}
	}

	return out
}

// outageSpans works out the spans of a window.
//
// A lease is a span from its grant to its end (or to now): upgradoor took the
// host down. The attestations the host missed from the grant until one epoch
// after the end are the lease's.
//
// Misses that no lease explains make a span when a host missed attestations
// in at least outageMinSlots slots that lie no more than one epoch apart: a
// validator attests once per epoch, so a host that is back shows no miss for
// longer than that.
func outageSpans(in *outageInput) []Outage {
	epochSlots := max(in.health.SlotsPerEpoch, 1)
	slotTime := func(slot uint64) time.Time {
		at, _ := in.health.SlotStart(slot)

		return at.UTC()
	}
	windowStart := slotTime(in.first)

	out := make([]Outage, 0, len(in.leases)+8)
	// byHost indexes the lease spans of a host in out.
	byHost := make(map[string][]int, len(in.leases))

	for _, l := range in.leases {
		end := leaseEnd(l, in.now)
		if !end.After(windowStart) {
			continue
		}

		span := Outage{
			Hosts: 1, Names: []string{l.Host},
			From: l.GrantedAt.UTC(), To: end.UTC(),
			FromStart: l.GrantedAt.Before(windowStart), Open: !l.State.Terminal(),
			Lease: l.ID, Work: leaseWork(l), Rollout: leaseRollout(l),
			Failed:     l.State == fleet.LeaseFailed || l.State == fleet.LeaseExpired,
			Deployed:   leaseDeployed(l),
			Validators: in.validators[l.Host],
		}

		if span.Validators == 0 {
			span.Validators = l.Weight
		}

		byHost[l.Host] = append(byHost[l.Host], len(out))
		out = append(out, span)
	}

	// The misses no lease explains, per host, in the order of the slots.
	loose := make(map[string][]fleet.HostSlotRun, 8)
	hosts := make([]string, 0, 8)

	for i := range in.runs {
		run := in.runs[i]
		run.First, run.Last = max(run.First, in.first), min(run.Last, in.last)

		if run.First > run.Last || run.Included >= run.Assigned {
			continue
		}

		if explainRun(out, byHost[run.Host], &run, slotTime(run.First), in.health.EpochDuration()) {
			continue
		}

		if _, seen := loose[run.Host]; !seen {
			hosts = append(hosts, run.Host)
		}

		loose[run.Host] = append(loose[run.Host], run)
	}

	slices.Sort(hosts)

	for _, host := range hosts {
		rows := loose[host]
		slices.SortFunc(rows, func(a, b fleet.HostSlotRun) int { return cmp.Compare(a.First, b.First) })

		for start := 0; start < len(rows); {
			end := start + 1
			slots := rows[start].Slots

			for end < len(rows) && rows[end].First-rows[end-1].Last <= epochSlots {
				slots += rows[end].Slots
				end++
			}

			if slots >= outageMinSlots {
				run := rows[start:end]
				span := Outage{
					Hosts: 1, Names: []string{host},
					From: slotTime(run[0].First), To: slotTime(run[len(run)-1].Last + 1),
					// Within an epoch of an edge of the window the host may
					// have been off before it, or may still be.
					FromStart:  run[0].First-in.first < epochSlots,
					Open:       in.last-run[len(run)-1].Last <= epochSlots+2,
					Validators: in.validators[host],
				}

				for i := range run {
					span.Missed += run[i].Assigned - run[i].Included
					span.Assigned += run[i].Assigned
				}

				out = append(out, span)
			}

			start = end
		}
	}

	sortOutages(out)

	return out
}

// explainRun adds a run of one slot to the lease span of its host that
// covers the slot (until an epoch after the lease ended), if there is one.
// The runs of a host with a lease are single slots (outageInput.runs).
func explainRun(out []Outage, spans []int, run *fleet.HostSlotRun, at time.Time, epoch time.Duration) bool {
	for _, idx := range spans {
		span := &out[idx]

		until := span.To
		if !span.Open {
			until = until.Add(epoch)
		}

		if !at.Before(span.From) && at.Before(until) {
			span.Missed += run.Assigned - run.Included
			span.Assigned += run.Assigned

			return true
		}
	}

	return false
}

// leaseEnd is when a lease ended; now for one that is open.
func leaseEnd(l *fleet.Lease, now time.Time) time.Time {
	switch {
	case l.EndedAt != nil:
		return *l.EndedAt
	case l.State.Terminal():
		// Ended, and nobody wrote down when: it took no time.
		return l.GrantedAt
	default:
		return now
	}
}

// leaseHosts names the hosts with a lease that reaches into the window from
// windowStart on.
func leaseHosts(leases []*fleet.Lease, windowStart, now time.Time) []string {
	out := make([]string, 0, len(leases))

	for _, l := range leases {
		if leaseEnd(l, now).After(windowStart) && !slices.Contains(out, l.Host) {
			out = append(out, l.Host)
		}
	}

	return out
}

func sortOutages(spans []Outage) {
	slices.SortFunc(spans, func(a, b Outage) int {
		return cmp.Or(a.From.Compare(b.From), cmp.Compare(a.Names[0], b.Names[0]), cmp.Compare(a.Lease, b.Lease))
	})
}

// outageKind is what makes two spans the same kind of thing: spans of one
// kind that overlap can stand as one span for several hosts.
type outageKind struct {
	work, rollout string
	failed, whole bool
}

func kindOf(o *Outage) outageKind {
	return outageKind{work: o.Work, rollout: o.Rollout, failed: o.Failed, whole: o.FromStart && o.Open}
}

// capOutages bounds the spans of an answer: while there are more than limit,
// spans of one kind that overlap, or lie closer together than a gap that
// grows, are merged into one that says how many hosts it stands for. A window
// in which a thousand hosts were rolled a hundred at a time comes out as a
// few dozen spans.
func capOutages(spans []Outage, limit int, window time.Duration) []Outage {
	for _, gap := range []time.Duration{0, window / 200, window / 50, window / 10, window} {
		if len(spans) <= limit {
			break
		}

		spans = mergeOutages(spans, gap)
	}

	return spans
}

// mergeOutages merges the spans of one kind that are no further apart than
// gap.
func mergeOutages(spans []Outage, gap time.Duration) []Outage {
	slices.SortStableFunc(spans, func(a, b Outage) int {
		ka, kb := kindOf(&a), kindOf(&b)

		return cmp.Or(
			cmp.Compare(ka.work, kb.work), cmp.Compare(ka.rollout, kb.rollout),
			cmp.Compare(btoi(ka.failed), btoi(kb.failed)), cmp.Compare(btoi(ka.whole), btoi(kb.whole)),
			a.From.Compare(b.From),
		)
	})

	out := make([]Outage, 0, len(spans))

	for i := range spans {
		span := spans[i]

		if n := len(out); n > 0 && kindOf(&out[n-1]) == kindOf(&span) && !span.From.After(out[n-1].To.Add(gap)) {
			into := &out[n-1]
			into.Hosts += span.Hosts
			into.Names = mergeNames(into.Names, span.Names)
			into.Lease = ""
			into.Missed += span.Missed
			into.Assigned += span.Assigned
			into.Validators += span.Validators
			into.Open = into.Open || span.Open
			into.Deployed = into.Deployed || span.Deployed

			if span.To.After(into.To) {
				into.To = span.To
			}

			continue
		}

		out = append(out, span)
	}

	sortOutages(out)

	return out
}

func btoi(b bool) int {
	if b {
		return 1
	}

	return 0
}

// mergeNames joins two ordered lists of names, up to maxOutageNames.
func mergeNames(a, b []string) []string {
	out := make([]string, 0, min(len(a)+len(b), maxOutageNames))
	out = append(out, a...)

	for _, name := range b {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}

	slices.Sort(out)

	return out[:min(len(out), maxOutageNames)]
}
