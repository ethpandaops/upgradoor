package scheduler

import (
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Ranks of one client of a node, worst first.
const (
	nodeUnreachable = iota
	nodeELOffline
	nodeSyncing
	nodeShortOfPeers
	nodeOK
)

var nodeRankWords = [...]string{"not reachable", "reports its EL offline", "syncing", "short of peers", "ok"}

// elRank ranks the execution client.
func elRank(el *protocol.ELHealth, minPeers int) int {
	switch {
	case !el.Reachable:
		return nodeUnreachable
	case el.Syncing:
		return nodeSyncing
	case el.Peers < minPeers:
		return nodeShortOfPeers
	default:
		return nodeOK
	}
}

// clRank ranks the consensus client.
func clRank(cl *protocol.CLHealth, minPeers int) int {
	switch {
	case !cl.Reachable:
		return nodeUnreachable
	case cl.ELOffline:
		return nodeELOffline
	case cl.Syncing:
		return nodeSyncing
	case cl.Peers < minPeers:
		return nodeShortOfPeers
	default:
		return nodeOK
	}
}

// readiness returns the component a host's unreadiness starts at and what it
// is, from its last sync: el when the EL is not ok, else cl when the CL is
// not, else the first probed kind that is not healthy, else vc when most of
// its validators with a duty did not attest. Both are empty for a host that
// is ready, stale or has not synced.
func (p *pass) readiness(hv *hostView) (root protocol.ContainerKind, unready string) {
	st := hv.h.Status
	if st == nil || hv.stale {
		return "", ""
	}

	minPeers := p.minPeers()
	node := st.Node

	if missing := node.Missing(minPeers); missing != "" {
		if node.EL != nil && elRank(node.EL, minPeers) < nodeOK {
			return protocol.KindEL, missing
		}

		return protocol.KindCL, missing
	}

	for _, probe := range node.Probes {
		if !probe.Healthy {
			return probe.Kind, node.KindMissing(probe.Kind)
		}
	}

	// Scattered misses are a devnet's normal: only most of them missing is.
	if hv.duty > 0 && hv.live*2 < hv.duty {
		return protocol.KindVC, fmt.Sprintf("validators: %d of %d attesting", hv.live, hv.duty)
	}

	return "", ""
}

// baseline is the host's state for a lease granted now, from its last sync;
// nil when that sync is older than half of hostStaleAfter.
func (p *pass) baseline(hv *hostView) *fleet.LeaseBaseline {
	st := hv.h.Status
	if st == nil || hv.stale || p.now.Sub(hv.h.LastSeen) > p.set.HostStaleAfter.Std()/2 {
		return nil
	}

	b := &fleet.LeaseBaseline{
		At: hv.h.LastSeen, Node: cloneNode(st.Node),
		Duty: hv.duty, Live: hv.live, LiveKnown: hv.liveKnown,
		Root: hv.root, Unready: hv.unready,
	}

	for i := range st.Containers {
		if c := &st.Containers[i]; c.Managed {
			b.Containers = append(b.Containers, fleet.BaselineContainer{
				Name: c.Name, Kind: c.Kind, State: c.State, RestartCount: c.RestartCount,
			})
		}
	}

	if _, below := p.comparePeers(st.Node); below != "" {
		b.BelowFleet = below
		b.Unready = cmpOr(b.Unready, below)
	}

	return b
}

func cloneNode(n protocol.NodeHealth) protocol.NodeHealth {
	c := protocol.NodeHealth{Probes: slices.Clone(n.Probes)}

	if n.EL != nil {
		el := *n.EL
		c.EL = &el
	}

	if n.CL != nil {
		cl := *n.CL
		c.CL = &cl
	}

	return c
}

// lenientCheck reports whether the lease's host was not ready before it:
// its node work is then held to no worse than its baseline once
// baselineGrace has passed.
func lenientCheck(l *fleet.Lease) bool {
	return l.Baseline != nil && l.Baseline.Unready != "" && leaseImpact(l) == protocol.ImpactNode
}

// noWorse judges a verifying lease that failed the readiness check against
// its baseline, keeps the run of syncs that were no worse, and says whether
// the lease is released as no worse. worse is the first signal that is worse
// than in the baseline; note is the readiness note with that judgement.
func (p *pass) noWorse(
	l *fleet.Lease, hv *hostView, strict string, peers func() string,
) (release bool, worse, note string) {
	b := l.Baseline
	prefix := strict + "; not ready before the lease (" + b.Unready + "): "
	seen := hv.h.LastSeen

	switch {
	case hv.h.Status == nil || !seen.After(*l.AgentDoneAt):
		endNoWorse(l)

		return false, "", prefix + noteAwaitSync
	case seen.Before(p.w.Since):
		endNoWorse(l)

		return false, "", prefix + noteAwaitSyncStart
	case hv.stale:
		endNoWorse(l)

		return false, "", prefix + reasonAgentSilent
	}

	worse, pending := p.compareBaseline(l, hv, peers)

	switch {
	case worse != "":
		endNoWorse(l)

		return false, worse, prefix + "worse: " + worse
	case pending != "":
		endNoWorse(l)

		return false, "", prefix + pending
	}

	if l.NoWorseSince != nil && (l.NoWorseSince.Before(p.w.Since) ||
		seen.Sub(*l.NoWorseUntil) > p.set.HostStaleAfter.Std()) {
		endNoWorse(l)
	}

	if l.NoWorseSince == nil {
		since := seen
		l.NoWorseSince = &since
	}

	until := seen
	l.NoWorseUntil = &until

	graceEnd := later(*l.AgentDoneAt, p.w.Since).Add(p.set.BaselineGrace.Std())
	stableAt := l.NoWorseSince.Add(p.set.ReadyStableFor.Std())

	if !p.now.Before(graceEnd) && !seen.Before(stableAt) {
		return true, "", prefix + "no worse"
	}

	return false, "", prefix + "no worse, released as such from " +
		later(graceEnd, stableAt).UTC().Format(time.TimeOnly) + " if it stays so"
}

// compareBaseline compares the host's last sync with the lease's baseline.
// worse names a signal that is worse than in the baseline; pending one that
// was fine in the baseline and must be again, as the full check asks.
func (p *pass) compareBaseline(l *fleet.Lease, hv *hostView, peers func() string) (worse, pending string) {
	b := l.Baseline
	node := hv.h.Status.Node
	minPeers := p.minPeers()

	if was := b.Node.EL; was != nil {
		now := node.EL
		if now == nil {
			return "the EL probe is gone", ""
		}

		if r, r0 := elRank(now, minPeers), elRank(was, minPeers); r < r0 {
			return "EL " + nodeRankWords[r] + ", was " + nodeRankWords[r0], ""
		}

		if now.Reachable && was.Reachable && now.Head < was.Head {
			return fmt.Sprintf("EL head %d, was %d", now.Head, was.Head), ""
		}
	}

	if was := b.Node.CL; was != nil {
		now := node.CL
		if now == nil {
			return "the CL probe is gone", ""
		}

		if r, r0 := clRank(now, minPeers), clRank(was, minPeers); r < r0 {
			return "CL " + nodeRankWords[r] + ", was " + nodeRankWords[r0], ""
		}

		if now.Reachable && was.Reachable && now.HeadSlot < was.HeadSlot {
			return fmt.Sprintf("CL head slot %d, was %d", now.HeadSlot, was.HeadSlot), ""
		}
	}

	for _, kind := range leaseKinds(l, hv.h.Status) {
		if missing := node.KindMissing(kind); missing != "" && b.Node.KindMissing(kind) == "" {
			return missing + ", healthy before", ""
		}
	}

	// Validators that did not attest before need not attest now; others
	// must, as the full check asks.
	if hv.weight > 0 && (!b.LiveKnown || b.Duty == 0 || b.Live > 0) {
		if ok, note := p.validatorsReady(l, hv); !ok {
			return "", note
		}
	}

	if b.BelowFleet == "" {
		if below := peers(); below != "" {
			return "", below
		}
	}

	return "", ""
}

// leaseKinds are the kinds of the managed containers the lease's needs are
// about, in the order of its needs.
func leaseKinds(l *fleet.Lease, st *protocol.SyncRequest) []protocol.ContainerKind {
	kinds := make([]protocol.ContainerKind, 0, len(l.Needs))

	for i := range l.Needs {
		if kind := containerKind(st, l.Needs[i].Container()); kind != "" && !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}

	return kinds
}

// containerKind is the kind of the named managed container on the host;
// empty for a need about no container and for a container it does not
// report.
func containerKind(st *protocol.SyncRequest, name string) protocol.ContainerKind {
	if name == "" || st == nil {
		return ""
	}

	for i := range st.Containers {
		if c := &st.Containers[i]; c.Name == name && c.Managed {
			return c.Kind
		}
	}

	return ""
}

// endNoWorse ends the lease's run of syncs that were no worse.
func endNoWorse(l *fleet.Lease) {
	l.NoWorseSince = nil
	l.NoWorseUntil = nil
}

// Places of a member in the serving order of its rollout (servingRank).
const (
	rankDownRelated = iota
	rankHealthy
	rankDownUnrelated
)

// servingRank places a host in a rollout: a host that is not ready for
// something the rollout's needs on it are about goes first (the change may
// be what brings it back), a ready host next, and a host not ready for
// something else last: serving it proves little and its readiness check is
// lenient.
func (p *pass) servingRank(r *fleet.Rollout, hv *hostView) int {
	if hv.root == "" {
		return rankHealthy
	}

	for _, n := range p.needs[r.ID][hv.h.Name] {
		kind := containerKind(hv.h.Status, n.Container())
		if kind == "" {
			continue
		}

		if kind == hv.root || (hv.root == protocol.KindVC && kind == protocol.KindCL) {
			return rankDownRelated
		}
	}

	return rankDownUnrelated
}
