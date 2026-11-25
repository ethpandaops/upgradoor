package views

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Access is how much of the hosts a login reads (authz.FeatureDetails).
// A host in full is what the views build. A reduced host keeps its name,
// roles, validators, liveness, holds, plan, verbs, the kinds of its needs
// and its managed containers by image; it loses its OS and kernel, its
// package counts and inventory, its unmanaged containers, the internals of
// managed containers that no team of the login owns, its address,
// certificate and agent, its uptime, resource use and node probes, and its
// host actions.
//
// The audit log is reduced the same way, and without the feature admin it
// leaves out what only the admin pages show: the settings in a settings
// change but what it changed, the error of a fetch of the validator ranges.
// The zero Access reduces every host and owns nothing.
type Access struct {
	// Full shows every host in full: the plain feature, or the API runs
	// open.
	Full bool
	// Admin shows the audit log in full: the feature admin, or the API runs
	// open.
	Admin bool
	// Teams are the login's teams: their containers are shown in full on
	// every host.
	Teams []string
	// OwnedHosts shows a host in full when one of Teams owns a managed
	// container on it (the team twin).
	OwnedHosts bool
}

// Reduced rollout and audit texts.
const (
	reducedPackagesTitle = "packages"
)

// auditHostKeys are the audit detail keys that carry a host's identity or
// the internals of a container: left out of a reduced audit entry.
var auditHostKeys = []string{
	"certHash", "remoteAddr", "addFlags", "removeFlags",
}

// Audit targets and keys that only the admin reads in full.
const (
	auditTargetSettings = "settings"
	auditActionRanges   = "sources.validator-ranges.refresh"
	auditKeyBefore      = "before"
	auditKeyAfter       = "after"
	auditKeyError       = "error"
)

// Key names the access, for views built once per access. Equal keys reduce
// alike.
func (a *Access) Key() string {
	admin := ""
	if a.Admin {
		admin = "+admin"
	}

	if a.Full {
		return "full" + admin
	}

	key := "reduced" + admin
	if a.OwnedHosts {
		key += "+hosts"
	}

	teams := slices.Clone(a.Teams)
	slices.Sort(teams)

	return key + ":" + strings.Join(slices.Compact(teams), "\x00")
}

// ownsTeam reports whether team is one of the login's teams.
func (a *Access) ownsTeam(team string) bool {
	return team != "" && slices.Contains(a.Teams, team)
}

// owns reports whether a team of the login owns a managed container.
func (a *Access) owns(c *protocol.Container) bool {
	return c.Managed && a.ownsTeam(c.Team)
}

// HostFull reports whether a host with these containers is shown in full.
func (a *Access) HostFull(containers []protocol.Container) bool {
	if a.Full {
		return true
	}

	if !a.OwnedHosts {
		return false
	}

	for i := range containers {
		if a.owns(&containers[i]) {
			return true
		}
	}

	return false
}

// Summary is a host row as the login sees it.
func (a *Access) Summary(sum *HostSummary) HostSummary {
	if a.HostFull(sum.Containers) {
		return *sum
	}

	out := *sum
	out.AgentVersion = ""
	out.Node = protocol.NodeHealth{}
	out.OS = protocol.OSSummary{}
	out.Pending = protocol.Pending{}
	out.UptimeSeconds = 0
	out.Resources = nil
	out.Actions = []protocol.ActionInfo{}
	out.Containers = a.containers(sum.Containers)
	out.Needs = a.needs(sum.Needs)

	return out
}

// Detail is a host's details as the login sees them; d itself when it sees
// the host in full, nil for nil.
func (a *Access) Detail(d *HostDetail) *HostDetail {
	if d == nil || a.HostFull(d.Containers) {
		return d
	}

	out := &HostDetail{
		HostSummary: a.Summary(&d.HostSummary),
		Assigned:    d.Assigned,
		Leases:      make([]*fleet.Lease, 0, len(d.Leases)),
		History:     a.Audit(d.History),
		Reduced:     true,
	}

	for _, l := range d.Leases {
		out.Leases = append(out.Leases, a.Lease(l))
	}

	return out
}

// containers keeps the managed containers: in full when a team of the login
// owns them, by image otherwise.
func (a *Access) containers(in []protocol.Container) []protocol.Container {
	out := make([]protocol.Container, 0, len(in))

	for i := range in {
		c := &in[i]

		switch {
		case !c.Managed:
		case a.owns(c):
			out = append(out, *c)
		default:
			out = append(out, ReducedContainer(c))
		}
	}

	return out
}

// ReducedContainer is a managed container without its internals: what
// it runs and how the controller rolls it.
func ReducedContainer(c *protocol.Container) protocol.Container {
	out := protocol.Container{
		Name: c.Name, Image: c.Image, Digest: c.Digest, Revision: c.Revision, Version: c.Version,
		State: c.State, Health: c.Health, Managed: c.Managed, Kind: c.Kind, Team: c.Team,
		NotRolled: c.NotRolled, Impact: c.Impact, ConfiguredImage: c.ConfiguredImage,
	}

	if c.Registry != nil {
		reg := *c.Registry
		out.Registry = &reg
	}

	return out
}

// needs keeps the needs of containers a team of the login owns in full and
// takes the host-level details (packages, host actions, pull errors) out of
// the others.
func (a *Access) needs(in []protocol.Need) []protocol.Need {
	if in == nil {
		return nil
	}

	out := make([]protocol.Need, 0, len(in))

	for i := range in {
		n := in[i]

		if !a.ownsTeam(needTeam(&n)) {
			n.Packages, n.Action, n.PullError = nil, nil, ""
		}

		out = append(out, n)
	}

	return out
}

// needTeam is the team of the container a need is about, as far as the need
// says.
func needTeam(n *protocol.Need) string {
	switch {
	case n.Image != nil:
		return n.Image.Team
	case n.Spec != nil:
		return n.Spec.Team
	default:
		return ""
	}
}

// Lease is a lease as a login without the full view sees it: its needs
// reduced, no host action output, and no step error of a need that no team
// of the login owns.
func (a *Access) Lease(l *fleet.Lease) *fleet.Lease {
	if a.Full {
		return l
	}

	out := *l
	out.Needs = a.needs(l.Needs)

	if l.Baseline != nil {
		baseline := *l.Baseline
		baseline.Node = nodeWithoutErrors(l.Baseline.Node)
		out.Baseline = &baseline
	}

	if l.Operation != nil {
		op := *l.Operation
		op.Steps = make([]protocol.Step, 0, len(l.Operation.Steps))

		for _, step := range l.Operation.Steps {
			if step.Action != nil {
				result := *step.Action
				result.Output = ""
				step.Action = &result
			}

			if !a.ownsNeed(l.Needs, step.NeedID) {
				step.Error = ""
			}

			op.Steps = append(op.Steps, step)
		}

		out.Operation = &op
	}

	return &out
}

// nodeWithoutErrors is the node's state without the probes' error texts,
// which name the host's internals.
func nodeWithoutErrors(n protocol.NodeHealth) protocol.NodeHealth {
	out := protocol.NodeHealth{Probes: make([]protocol.KindHealth, 0, len(n.Probes))}

	if n.EL != nil {
		el := *n.EL
		el.Error = ""
		out.EL = &el
	}

	if n.CL != nil {
		cl := *n.CL
		cl.Error = ""
		out.CL = &cl
	}

	for _, p := range n.Probes {
		p.Error = ""
		out.Probes = append(out.Probes, p)
	}

	return out
}

// ownsNeed reports whether a team of the login owns the container the need
// id of needs is about.
func (a *Access) ownsNeed(needs []protocol.Need, id string) bool {
	for i := range needs {
		if needs[i].ID == id {
			return a.ownsTeam(needTeam(&needs[i]))
		}
	}

	return false
}

// Rollout is a rollout as a login without the full view sees it: a packages
// rollout without its packages.
func (a *Access) Rollout(r *RolloutView) *RolloutView {
	if a.Full || r.Kind != fleet.RolloutPackages {
		return r
	}

	out := *r
	out.Packages = nil
	out.Title = reducedPackagesTitle

	return &out
}

// Rollouts is the rollout view as the login sees it.
func (a *Access) Rollouts(in *Rollouts) *Rollouts {
	if a.Full {
		return in
	}

	rollouts := func(list []*RolloutView) []*RolloutView {
		out := make([]*RolloutView, 0, len(list))
		for _, r := range list {
			out = append(out, a.Rollout(r))
		}

		return out
	}

	leases := func(list []*fleet.Lease) []*fleet.Lease {
		out := make([]*fleet.Lease, 0, len(list))
		for _, l := range list {
			out = append(out, a.Lease(l))
		}

		return out
	}

	return &Rollouts{
		Open: rollouts(in.Open), Recent: rollouts(in.Recent),
		Leases: leases(in.Leases), RecentLeases: leases(in.RecentLeases),
	}
}

// Audit is audit entries as the login sees them. Without the full view:
// without host identities, flag changes and the package list of a rollout's
// title. Without admin: what adminAuditDetail leaves out.
func (a *Access) Audit(in []fleet.AuditEntry) []fleet.AuditEntry {
	if a.Full && a.Admin {
		return in
	}

	out := make([]fleet.AuditEntry, 0, len(in))

	for _, e := range in {
		if !a.Admin {
			e.Detail = adminAuditDetail(&e)
		}

		if !a.Full {
			e.Detail = reduceAuditDetail(e.Action, e.Detail)
		}

		out = append(out, e)
	}

	return out
}

// adminAuditDetail is the detail of an entry without what only the admin
// reads: of a settings change the settings it changed, before and after,
// and not the others; of a fetch of the validator ranges not its error,
// which can name the inventory.
func adminAuditDetail(e *fleet.AuditEntry) map[string]any {
	switch {
	case e.Detail == nil:
		return nil
	case e.Target == auditTargetSettings:
		before, after, ok := settingsChange(e.Detail)
		if !ok {
			return e.Detail
		}

		out := maps.Clone(e.Detail)
		out[auditKeyBefore], out[auditKeyAfter] = before, after

		return out
	case e.Action == auditActionRanges:
		out := maps.Clone(e.Detail)
		delete(out, auditKeyError)

		return out
	default:
		return e.Detail
	}
}

// settingsChange cuts the before and after of a settings change down to the
// settings that differ.
func settingsChange(detail map[string]any) (before, after map[string]any, ok bool) {
	b, okBefore := asObject(detail[auditKeyBefore])
	f, okAfter := asObject(detail[auditKeyAfter])

	if !okBefore || !okAfter {
		return nil, nil, false
	}

	before = make(map[string]any, 4)
	after = make(map[string]any, 4)

	for key, vb := range b {
		va, inAfter := f[key]
		if inAfter && reflect.DeepEqual(vb, va) {
			continue
		}

		before[key] = vb

		if inAfter {
			after[key] = va
		}
	}

	for key, va := range f {
		if _, inBefore := b[key]; !inBefore {
			after[key] = va
		}
	}

	return before, after, true
}

// asObject reads an audit detail value as a JSON object: a map once the
// entry was stored, a struct (fleet.Settings) while it is fresh.
func asObject(v any) (map[string]any, bool) {
	if m, ok := v.(map[string]any); ok {
		return m, true
	}

	if v == nil {
		return nil, false
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}

	return out, true
}

func reduceAuditDetail(action string, in map[string]any) map[string]any {
	if in == nil {
		return nil
	}

	out := make(map[string]any, len(in))

	for k, v := range in {
		if slices.Contains(auditHostKeys, k) {
			continue
		}

		out[k] = v
	}

	switch action {
	case "host.certificate-changed":
		delete(out, auditKeyBefore)
		delete(out, auditKeyAfter)
	case "rollout.created":
		if title, ok := out["title"].(string); ok && strings.HasPrefix(title, reducedPackagesTitle+":") {
			out["title"] = reducedPackagesTitle
		}
	}

	return out
}

// Clients is the client list as the login sees it: the configured command
// line only of the clients a team of the login owns.
func (a *Access) Clients(in []ClientView) []ClientView {
	if a.Full {
		return in
	}

	out := make([]ClientView, 0, len(in))

	for i := range in {
		v := in[i]

		if !a.ownsTeam(v.Name) {
			images := make([]ClientImage, 0, len(v.Images))

			for j := range v.Images {
				img := v.Images[j]
				img.Command, img.CommonArgs = nil, nil
				images = append(images, img)
			}

			v.Images = images
		}

		out = append(out, v)
	}

	return out
}

// ClientHost is a host on a client's page as the login sees it.
func (a *Access) ClientHost(row *ClientHost) ClientHost {
	if a.Full {
		return *row
	}

	out := *row
	if !a.HostFull(row.Containers) {
		out.Usage = nil
	}

	out.Containers = a.containers(row.Containers)

	return out
}
