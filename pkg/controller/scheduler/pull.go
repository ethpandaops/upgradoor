package scheduler

import (
	"cmp"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// A host pulls the images its needs wait for (protocol.Need.Unpulled) only
// inside a pull window the scheduler grants: at most maxPullingPercent of the
// hosts at once, hosts near their slot first (the serving order), and only
// for rollouts that may be served (active, not paused, approved where their
// policy is manual). A window names exactly the images of the host's needs in
// those rollouts. A need that waits for an image is not granted.

// pullWindowTTL bounds a window: the agent gives a pull 30 minutes.
const pullWindowTTL = 40 * time.Minute

// Pull states of a host in the plan.
const (
	// PullWaiting: the host's needs wait for images and it has no window.
	PullWaiting = "waiting"
	// PullPulling: the host has a window.
	PullPulling = "pulling"
	// PullPulled: the host waits in a rollout with its images local.
	PullPulled = "pulled"
)

// reasonPull is the waiting reason of a host whose needs wait for images.
const reasonPull = "waiting for its images to be pulled"

// unpulled reports whether any of the needs waits for an image.
func unpulled(needs []protocol.Need) bool {
	return slices.ContainsFunc(needs, func(n protocol.Need) bool { return len(n.Unpulled) > 0 })
}

// pullWants returns the images the host's needs wait for in the rollouts
// that may be served, sorted, and whether it waits in any such rollout.
func (p *pass) pullWants(hv *hostView) (images []protocol.PullImage, waits bool) {
	for _, r := range p.rollouts {
		if r.State.Terminal() || p.policyVerdict(r).rank != rankGrantable {
			continue
		}

		m := p.member(r, hv.h.Name)
		if m == nil || m.State != fleet.HostPending {
			continue
		}

		waits = true

		for _, n := range p.needs[r.ID][hv.h.Name] {
			for _, img := range n.Unpulled {
				if !slices.Contains(images, img) {
					images = append(images, img)
				}
			}
		}
	}

	slices.SortFunc(images, func(a, b protocol.PullImage) int {
		return cmp.Or(cmp.Compare(a.Ref, b.Ref), cmp.Compare(a.Digest, b.Digest))
	})

	return images, waits
}

// reportsPulling reports whether the host's agent said in its last sync, not
// longer than a window ago, that it is pulling: it counts for the limit
// whatever the scheduler thinks of its window.
func (p *pass) reportsPulling(hv *hostView) bool {
	return hv.h.Status != nil && len(hv.h.Status.Pulling) > 0 && p.now.Sub(hv.h.LastSeen) < pullWindowTTL
}

// pulls keeps, ends and grants the pull windows of this pass and notes each
// host's pull state in the plan. It runs after the grants.
func (p *pass) pulls(plan *Plan) []store.HostPull {
	limit := hostLimit(p.set.MaxPullingPercent, len(p.hosts))
	wants := make(map[string][]protocol.PullImage, len(p.hosts))
	busy := 0

	var out []store.HostPull

	state := func(name, s string) {
		hp := plan.Hosts[name]
		hp.Pull = s
		plan.Hosts[name] = hp
	}

	for _, name := range p.names {
		hv := p.hosts[name]
		images, waits := p.pullWants(hv)
		wants[name] = images

		change, keep, counted := p.settleWindow(hv, images)
		if change != nil {
			out = append(out, *change)
		}

		switch {
		case keep:
			state(name, PullPulling)
		case len(images) > 0:
			state(name, PullWaiting)
		case waits && hv.lease == nil:
			state(name, PullPulled)
		}

		if counted || p.reportsPulling(hv) {
			busy++
		}
	}

	for _, wt := range p.pullOrder() {
		if busy >= limit {
			break
		}

		name := wt.hv.h.Name

		if plan.Hosts[name].Pull == PullPulling || len(wants[name]) == 0 ||
			wt.hv.held || wt.hv.stale || p.reportsPulling(wt.hv) {
			continue
		}

		out = append(out, store.HostPull{
			Host: name, Window: &fleet.PullWindow{Images: wants[name], GrantedAt: p.now},
		})
		state(name, PullPulling)

		busy++
	}

	return out
}

// settleWindow decides what becomes of a host's window, given the images its
// needs wait for now: kept (for those images), ended, or gone. counted says
// that it counts for the limit: until the agent has synced after its end
// without pulling, or the window's time is up.
func (p *pass) settleWindow(
	hv *hostView, images []protocol.PullImage,
) (change *store.HostPull, keep, counted bool) {
	w := hv.h.Pull
	if w == nil {
		return nil, false, false
	}

	name := hv.h.Name
	expired := p.now.Sub(w.GrantedAt) >= pullWindowTTL
	keep = w.EndedAt == nil && !expired && len(images) > 0 && !hv.held && !hv.stale

	switch {
	case keep && !slices.Equal(w.Images, images):
		// The images changed (a tag moved on): the same window, for them.
		return &store.HostPull{
			Host: name, Window: &fleet.PullWindow{Images: images, GrantedAt: w.GrantedAt},
		}, true, true
	case keep:
		return nil, true, true
	case w.EndedAt == nil && !expired:
		// Ended: the agent hears of it at its next sync. Until then it may
		// still pull, so the window counts on.
		ended := *w
		ended.EndedAt = &p.now

		return &store.HostPull{Host: name, Window: &ended}, false, true
	case expired || (hv.h.LastSeen.After(*w.EndedAt) && !p.reportsPulling(hv)):
		return &store.HostPull{Host: name}, false, false
	default:
		return nil, false, true
	}
}

// pullOrder is the order in which hosts get a pull window: the serving
// order, so hosts near their slot pull first.
func (p *pass) pullOrder() []*waiting {
	return p.servingOrder()
}
