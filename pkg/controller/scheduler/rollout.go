package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	keyReboot      = "reboot"
	groupPackages  = "packages"
	prefixImageKey = "image:"
	prefixPackages = "packages:"
	prefixManual   = "manual:"
)

// imageKey is the rollout key of an image reference at a digest.
func imageKey(image, digest string) string {
	return prefixImageKey + image + "@" + digest
}

// imageTarget is the digest an image reference is rolled to: the one named
// by the need with the freshest registry check.
type imageTarget struct {
	digest   string
	revision string
	checked  time.Time
}

// reportedNeeds is what the hosts' observed needs (those without a
// directive) ask for.
type reportedNeeds struct {
	images   map[string]*imageTarget
	packages map[string][]protocol.PackageChange
	reboot   bool
}

// syncDirectives takes over the agents' acknowledgements and keeps the hold
// directives in line with Host.HoldWanted.
func (p *pass) syncDirectives() {
	for _, d := range p.directives {
		if d.State != fleet.DirectivePending {
			continue
		}

		hv := p.hosts[d.Host]
		if hv == nil {
			d.State = fleet.DirectiveCancelled
			d.Error = reasonHostForgotten

			continue
		}

		if isHoldKind(d.Kind) || hv.h.Status == nil {
			continue
		}

		for _, ack := range hv.h.Status.DirectiveAcks {
			if ack.ID != d.ID {
				continue
			}

			d.State = fleet.DirectiveAccepted

			if ack.Status == protocol.DirectiveRejected {
				d.State = fleet.DirectiveRejected
				d.Error = cmpOr(ack.Error, "rejected by the agent")
			}

			break
		}
	}

	for _, name := range p.names {
		p.syncHold(p.hosts[name])
	}
}

func isHoldKind(k protocol.DirectiveKind) bool {
	return k == protocol.DirectiveHold || k == protocol.DirectiveReleaseHold
}

// syncHold issues hold / release-hold directives until the agent's reported
// controller hold matches Host.HoldWanted. They are settled by the reported
// state, not by acknowledgements.
func (p *pass) syncHold(hv *hostView) {
	if hv.h.Status == nil {
		return
	}

	kind := protocol.DirectiveReleaseHold
	if hv.h.HoldWanted {
		kind = protocol.DirectiveHold
	}

	agreed := hv.h.Status.Hold.Controller == hv.h.HoldWanted
	outstanding := false

	for _, d := range p.directives {
		if d.Host != hv.h.Name || d.State != fleet.DirectivePending || !isHoldKind(d.Kind) {
			continue
		}

		switch {
		case agreed && d.Kind == kind:
			d.State = fleet.DirectiveAccepted
		case agreed || d.Kind != kind:
			d.State = fleet.DirectiveCancelled
		default:
			outstanding = true
		}
	}

	if agreed || outstanding {
		return
	}

	p.directives = append(p.directives, &fleet.Directive{
		Directive: protocol.Directive{ID: p.w.NewID(), Kind: kind, Reason: hv.h.HoldReason},
		Host:      hv.h.Name,
		State:     fleet.DirectivePending,
		CreatedBy: actorScheduler,
		CreatedAt: p.now,
	})
}

// syncRollouts supersedes, opens, updates membership of and completes
// rollouts from the needs the hosts report.
func (p *pass) syncRollouts() {
	p.liftSkips()

	rep := p.reported()

	p.supersede(rep)

	created := p.open(rep)

	p.assignNeeds()

	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		p.syncMembers(r, created[r.ID])
		p.complete(r)
	}
}

// openKeys are the keys of the non-terminal rollouts.
func (p *pass) openKeys() map[string]bool {
	keys := make(map[string]bool, len(p.rollouts))

	for _, r := range p.rollouts {
		if !r.State.Terminal() {
			keys[r.Key] = true
		}
	}

	return keys
}

// skipNote returns the note of the skip that holds for the host in the
// latest closed rollout of the key, or "".
func (p *pass) skipNote(key, host string) string {
	r := p.closedLatest[key]
	if r == nil {
		return ""
	}

	if m := r.Host(host); m != nil && operatorSkipped(m) {
		return m.Error
	}

	return ""
}

// liftSkips ends the skips that outlived their need. A skip holds for the
// key of its rollout for as long as the host keeps reporting the need: once
// a host that is neither held nor silent syncs without it, a later need of
// the same key (the reboot key never changes) is a new one. While a rollout
// of the key is open the skip lives on in its member list instead.
func (p *pass) liftSkips() {
	open := p.openKeys()

	for _, key := range sortedKeys(p.closedLatest) {
		r := p.closedLatest[key]
		if open[key] || r.Kind == fleet.RolloutManual {
			continue
		}

		copied := false

		for i := range r.Hosts {
			m := &r.Hosts[i]

			hv := p.hosts[m.Host]
			if !operatorSkipped(m) || hv == nil || hv.h.Status == nil || hv.held || hv.stale ||
				p.hostKeys[m.Host][key] {
				continue
			}

			if !copied {
				r = cloneRollout(r)
				m = &r.Hosts[i]
				p.closedLatest[key] = r
				p.closedDirty = append(p.closedDirty, r)
				copied = true
			}

			m.Error = skipLiftedPrefix + reasonNeedGone
			m.UpdatedAt = p.now

			p.record("rollout.skip-lifted", r.ID, map[string]any{
				"host": m.Host, keyKey: key, keyReason: reasonNeedGone,
			})
		}
	}
}

// reported collects the observed needs of all hosts. The need of a host
// that a person skipped for the same key in its latest closed rollout is
// left out: it opens nothing.
func (p *pass) reported() *reportedNeeds {
	rep := &reportedNeeds{
		images:   make(map[string]*imageTarget, 4),
		packages: make(map[string][]protocol.PackageChange, 2),
	}

	incumbent := make(map[string]string, len(p.rollouts))

	for _, r := range p.rollouts {
		if r.Kind == fleet.RolloutImage && !r.State.Terminal() && incumbent[r.Image] == "" {
			incumbent[r.Image] = r.Digest
		}
	}

	open := p.openKeys()

	for _, name := range p.names {
		hv := p.hosts[name]

		for _, n := range hv.needs() {
			key := needKey(&n)
			if key == "" || (!open[key] && p.skipNote(key, name) != "") {
				continue
			}

			switch {
			case n.Kind == protocol.NeedImage && n.Image != nil:
				rep.image(n.Image, registryCheckedAt(hv.h, n.Image.Container), incumbent[n.Image.Image])
			case n.Kind == protocol.NeedPackages && n.Packages != nil:
				rep.packages[protocol.PackagesKey(n.Packages.Packages)] = n.Packages.Packages
			case n.Kind == protocol.NeedReboot:
				rep.reboot = true
			}
		}
	}

	return rep
}

// image offers one image need as the target of its reference. The freshest
// registry check wins; on a tie the digest of the open rollout stays, then
// the smaller digest.
func (rep *reportedNeeds) image(n *protocol.ImageNeed, checked time.Time, incumbent string) {
	cur := rep.images[n.Image]

	better := cur == nil
	if !better && n.ToDigest != cur.digest {
		switch c := checked.Compare(cur.checked); {
		case c != 0:
			better = c > 0
		case cur.digest == incumbent:
			better = false
		case n.ToDigest == incumbent:
			better = true
		default:
			better = n.ToDigest < cur.digest
		}
	}

	if better {
		rep.images[n.Image] = &imageTarget{digest: n.ToDigest, revision: n.ToRevision, checked: checked}
	}
}

// registryCheckedAt is when the host last looked at the tag of a container.
func registryCheckedAt(h *fleet.Host, container string) time.Time {
	if h.Status == nil {
		return time.Time{}
	}

	for i := range h.Status.Containers {
		c := &h.Status.Containers[i]
		if c.Name == container && c.Registry != nil {
			return c.Registry.CheckedAt
		}
	}

	return time.Time{}
}

// supersede closes image rollouts whose reference now has another target
// and packages rollouts whose key nobody reports while another key exists.
func (p *pass) supersede(rep *reportedNeeds) {
	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		switch r.Kind {
		case fleet.RolloutImage:
			if t := rep.images[r.Image]; t != nil && t.digest != r.Digest {
				p.carry(prefixImageKey+r.Image, r)
				p.closeRollout(r, fleet.RolloutSuperseded, "the tag moved to "+shortDigest(t.digest))
			}
		case fleet.RolloutPackages:
			if _, ok := rep.packages[strings.TrimPrefix(r.Key, prefixPackages)]; !ok && len(rep.packages) > 0 {
				p.carry(groupPackages, r)
				p.closeRollout(r, fleet.RolloutSuperseded, "newer package versions appeared")
			}
		default:
		}
	}
}

// carry remembers the hosts that had failed in a superseded rollout for the
// rollouts that follow it.
func (p *pass) carry(group string, r *fleet.Rollout) {
	set := p.carried[group]
	if set == nil {
		set = make(map[string]string, len(r.Hosts))
		p.carried[group] = set
	}

	for _, h := range carriedHosts(r) {
		set[h] = r.ID
	}
}

// carriedHosts lists the members that failed in the rollout, or that it had
// itself taken over as failed and not served yet.
func carriedHosts(r *fleet.Rollout) []string {
	out := make([]string, 0, len(r.Hosts))

	for i := range r.Hosts {
		m := &r.Hosts[i]

		waiting := m.State == fleet.HostPending || m.State == fleet.HostExpected
		if m.State == fleet.HostFailed || (waiting && strings.HasPrefix(m.Error, carriedPrefix)) {
			out = append(out, m.Host)
		}
	}

	return out
}

// carriedFor returns the hosts a new rollout of the group serves first.
func (p *pass) carriedFor(group string) map[string]string {
	if set, ok := p.carried[group]; ok {
		return set
	}

	for _, r := range p.w.Closed {
		if rolloutGroup(r) != group {
			continue
		}

		if r.State != fleet.RolloutSuperseded {
			return nil
		}

		set := make(map[string]string, len(r.Hosts))
		for _, h := range carriedHosts(r) {
			set[h] = r.ID
		}

		return set
	}

	return nil
}

// rolloutGroup names what a superseded rollout is followed by: rollouts of
// the same image reference, or of the packages kind.
func rolloutGroup(r *fleet.Rollout) string {
	switch r.Kind {
	case fleet.RolloutImage:
		return prefixImageKey + r.Image
	case fleet.RolloutPackages:
		return groupPackages
	default:
		return ""
	}
}

// closeRollout moves a rollout to a terminal state and cancels the manual
// directives nobody answered.
func (p *pass) closeRollout(r *fleet.Rollout, state fleet.RolloutState, reason string) {
	ended := p.now
	r.State = state
	r.Reason = reason
	r.EndedAt = &ended

	for _, d := range p.directives {
		if d.RolloutID == r.ID && d.State == fleet.DirectivePending {
			d.State = fleet.DirectiveCancelled
		}
	}

	p.record("rollout."+string(state), r.ID, map[string]any{keyKey: r.Key, keyReason: reason})
}

// open creates a rollout for every reported cause that has none, unless the
// latest closed rollout of its key was aborted. It returns the new rollouts
// with the hosts each serves first.
func (p *pass) open(rep *reportedNeeds) map[string]map[string]string {
	created := make(map[string]map[string]string, 2)
	openKeys := p.openKeys()

	add := func(r *fleet.Rollout) {
		if openKeys[r.Key] || p.abortedLatest(r.Key) {
			return
		}

		r.ID = p.w.NewID()
		r.State = fleet.RolloutActive
		r.CreatedBy = actorScheduler
		r.CreatedAt = p.now
		r.UpdatedAt = p.now

		p.rollouts = append(p.rollouts, r)
		p.rolloutByID[r.ID] = r
		openKeys[r.Key] = true

		carried := p.carriedFor(rolloutGroup(r))
		if carried == nil {
			carried = map[string]string{}
		}

		created[r.ID] = carried

		p.record("rollout.created", r.ID, map[string]any{keyKey: r.Key, "title": r.Title})
	}

	for _, image := range sortedKeys(rep.images) {
		t := rep.images[image]
		add(&fleet.Rollout{
			Kind:     fleet.RolloutImage,
			Key:      imageKey(image, t.digest),
			Title:    image + " to " + shortDigest(t.digest),
			Image:    image,
			Digest:   t.digest,
			Revision: t.revision,
		})
	}

	for _, key := range sortedKeys(rep.packages) {
		add(&fleet.Rollout{
			Kind:     fleet.RolloutPackages,
			Key:      prefixPackages + key,
			Title:    packagesTitle(rep.packages[key]),
			Packages: slices.Clone(rep.packages[key]),
		})
	}

	if rep.reboot {
		add(&fleet.Rollout{Kind: fleet.RolloutReboot, Key: keyReboot, Title: "reboot"})
	}

	return created
}

// abortedLatest reports whether the latest closed rollout of the key was
// aborted.
func (p *pass) abortedLatest(key string) bool {
	r := p.closedLatest[key]

	return r != nil && r.State == fleet.RolloutAborted
}

// assignNeeds sorts every reported need to the open rollout it belongs to.
func (p *pass) assignNeeds() {
	byKey := make(map[string]*fleet.Rollout, len(p.rollouts))

	for _, r := range p.rollouts {
		if !r.State.Terminal() {
			byKey[r.Key] = r
		}
	}

	manual := make(map[string]string, len(p.directives))

	for _, d := range p.directives {
		if d.RolloutID != "" {
			manual[d.ID] = d.RolloutID
		}
	}

	for _, name := range p.names {
		for _, n := range p.hosts[name].needs() {
			var r *fleet.Rollout

			switch {
			case n.DirectiveID != "":
				r = p.rolloutByID[manual[n.DirectiveID]]
			case n.Kind == protocol.NeedImage && n.Image != nil:
				r = byKey[imageKey(n.Image.Image, n.Image.ToDigest)]
			case n.Kind == protocol.NeedPackages && n.Packages != nil:
				r = byKey[prefixPackages+protocol.PackagesKey(n.Packages.Packages)]
			case n.Kind == protocol.NeedReboot:
				r = byKey[keyReboot]
			}

			if r == nil || r.State.Terminal() {
				continue
			}

			byHost := p.needs[r.ID]
			if byHost == nil {
				byHost = make(map[string][]protocol.Need, len(p.names))
				p.needs[r.ID] = byHost
			}

			byHost[name] = append(byHost[name], n)
		}
	}
}

// syncMembers updates the members of one open rollout. carried is non-nil
// for a rollout created in this pass.
func (p *pass) syncMembers(r *fleet.Rollout, carried map[string]string) {
	needs := p.needs[r.ID]

	for i := range r.Hosts {
		m := &r.Hosts[i]
		p.syncMember(r, m, needs[m.Host])
	}

	for _, name := range p.names {
		if r.Host(name) != nil {
			continue
		}

		hv := p.hosts[name]
		m := fleet.RolloutHost{Host: name, UpdatedAt: p.now}
		skip := p.skipNote(r.Key, name)

		switch {
		case skip != "":
			// A skip for the key outlives the rollout it was given in.
			m.State = fleet.HostSkipped
			m.Stage = hv.class()
			m.Error = skip

			if len(needs[name]) > 0 {
				m.NeedID = needs[name][0].ID
			}
		case len(needs[name]) > 0:
			m.State = fleet.HostPending
			m.NeedID = needs[name][0].ID
		case r.Kind == fleet.RolloutImage && expectsImage(r, hv):
			m.State = fleet.HostExpected
		default:
			continue
		}

		if prev, ok := carried[name]; ok && skip == "" {
			m.Error = carriedPrefix + prev
		}

		r.Hosts = append(r.Hosts, m)
	}

	// A canary that left without having been served (skipped, back to
	// expected, or done because its need went away by itself) proved
	// nothing: the next validator host granted takes its place.
	if c := r.Host(r.Canary); r.Canary != "" && (c == nil || c.State == fleet.HostSkipped ||
		c.State == fleet.HostExpected || (c.State == fleet.HostDone && c.LeaseID == "")) {
		r.Canary = ""

		if c != nil && c.Stage == fleet.StageCanary {
			c.Stage = fleet.StageBatches
		}
	}

	for i := range r.Hosts {
		m := &r.Hosts[i]

		hv := p.hosts[m.Host]
		if hv == nil || m.State.Settled() {
			continue
		}

		m.Stage = hv.class()
		if m.Stage == fleet.StageBatches && r.Canary == m.Host {
			m.Stage = fleet.StageCanary
		}
	}

	if carried != nil && r.Kind == fleet.RolloutImage {
		p.recheckExpected(r)
	}
}

// recheckExpected queues a recheck directive for every expected host of a
// new image rollout so it reports within one sync.
func (p *pass) recheckExpected(r *fleet.Rollout) {
	for i := range r.Hosts {
		m := &r.Hosts[i]
		if m.State != fleet.HostExpected {
			continue
		}

		if slices.ContainsFunc(p.directives, func(d *fleet.Directive) bool {
			return d.Host == m.Host && d.Kind == protocol.DirectiveRecheck &&
				d.State == fleet.DirectivePending
		}) {
			continue
		}

		p.directives = append(p.directives, &fleet.Directive{
			Directive: protocol.Directive{
				ID: p.w.NewID(), Kind: protocol.DirectiveRecheck, Reason: r.Title,
			},
			Host:      m.Host,
			State:     fleet.DirectivePending,
			CreatedBy: actorScheduler,
			CreatedAt: p.now,
		})
	}
}

// syncMember applies the membership rules to one existing member.
func (p *pass) syncMember(r *fleet.Rollout, m *fleet.RolloutHost, needs []protocol.Need) {
	hv := p.hosts[m.Host]
	if hv == nil {
		if !m.State.Settled() && m.State != fleet.HostLeased {
			p.setMember(m, fleet.HostSkipped)
			m.Error = reasonHostForgotten
		}

		return
	}

	switch m.State {
	case fleet.HostExpected:
		p.syncExpected(r, m, hv, needs)
	case fleet.HostPending:
		switch {
		case len(needs) > 0:
			m.NeedID = needs[0].ID
		case hv.h.Status == nil:
		case r.Kind == fleet.RolloutImage && expectsImage(r, hv):
			// The host no longer reports the need (it is held, or it looks
			// at an older digest) but still runs another digest.
			m.NeedID = ""
			p.setMember(m, fleet.HostExpected)
		default:
			// The need disappeared without a lease.
			m.LeaseID = ""
			p.setMember(m, fleet.HostDone)
		}
	case fleet.HostDone:
		// A host that was done without a lease and reports the need again
		// (a hold was lifted) rejoins.
		if m.LeaseID == "" && len(needs) > 0 {
			m.NeedID = needs[0].ID
			p.setMember(m, fleet.HostPending)
		}
	default:
	}
}

func (p *pass) syncExpected(
	r *fleet.Rollout, m *fleet.RolloutHost, hv *hostView, needs []protocol.Need,
) {
	outstanding, rejected := p.manualDirectives(r, m.Host)

	if len(needs) > 0 && !outstanding {
		m.NeedID = needs[0].ID
		p.setMember(m, fleet.HostPending)

		return
	}

	if hv.h.Status == nil {
		return
	}

	switch r.Kind {
	case fleet.RolloutImage:
		if !expectsImage(r, hv) && runsImage(r, hv) {
			p.setMember(m, fleet.HostDone)
		}
	case fleet.RolloutManual:
		if !outstanding && len(needs) == 0 && rejected != "" {
			p.setMember(m, fleet.HostSkipped)
			m.Error = rejected
		}
	default:
		if !hv.held && !hv.stale {
			p.setMember(m, fleet.HostDone)
		}
	}
}

// manualDirectives reports whether a directive of the rollout is still
// unanswered by the host, and the reason of the first one it rejected.
func (p *pass) manualDirectives(r *fleet.Rollout, host string) (outstanding bool, rejected string) {
	if r.Kind != fleet.RolloutManual {
		return false, ""
	}

	for _, d := range p.directives {
		if d.RolloutID != r.ID || d.Host != host {
			continue
		}

		switch d.State {
		case fleet.DirectivePending:
			outstanding = true
		case fleet.DirectiveRejected:
			if rejected == "" {
				rejected = cmpOr(d.Error, "rejected by the agent")
			}
		default:
		}
	}

	return outstanding, rejected
}

// expectsImage reports whether the host runs a managed, non-custom container
// on the rollout's image reference at another digest.
func expectsImage(r *fleet.Rollout, hv *hostView) bool {
	return hasContainer(r, hv, false)
}

// runsImage reports whether the host runs a managed, non-custom container on
// the rollout's image reference at its digest.
func runsImage(r *fleet.Rollout, hv *hostView) bool {
	return hasContainer(r, hv, true)
}

func hasContainer(r *fleet.Rollout, hv *hostView, onTarget bool) bool {
	if hv.h.Status == nil {
		return false
	}

	for i := range hv.h.Status.Containers {
		c := &hv.h.Status.Containers[i]
		if c.Managed && !c.Custom && c.Image == r.Image && (c.Digest == r.Digest) == onTarget {
			return true
		}
	}

	return false
}

// blockingExpected reports whether an expected member still holds back
// later stages and the completion of the rollout.
func (p *pass) blockingExpected(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	hv := p.hosts[m.Host]
	if hv == nil || hv.held || hv.stale {
		return false
	}

	if p.now.Sub(r.CreatedAt) > p.set.HostStaleAfter.Std() {
		return false
	}

	switch r.Kind {
	case fleet.RolloutImage:
		return expectsImage(r, hv) && hv.allows(protocol.VerbRoll)
	case fleet.RolloutManual:
		outstanding, _ := p.manualDirectives(r, m.Host)

		return outstanding
	default:
		return false
	}
}

// blocks reports whether a member is unsettled in a way that holds back
// later stages.
func (p *pass) blocks(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	switch m.State {
	case fleet.HostDone, fleet.HostSkipped:
		return false
	case fleet.HostExpected:
		return p.blockingExpected(r, m)
	default:
		return true
	}
}

// complete closes an active rollout nobody waits in any more.
func (p *pass) complete(r *fleet.Rollout) {
	if r.State != fleet.RolloutActive {
		return
	}

	for i := range r.Hosts {
		if p.blocks(r, &r.Hosts[i]) {
			return
		}
	}

	p.closeRollout(r, fleet.RolloutCompleted, "")
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}

	return d
}

func packagesTitle(pkgs []protocol.PackageChange) string {
	parts := make([]string, 0, len(pkgs))
	for _, c := range pkgs {
		parts = append(parts, fmt.Sprintf("%s %s", c.Name, c.To))
	}

	slices.Sort(parts)

	return "packages: " + strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
