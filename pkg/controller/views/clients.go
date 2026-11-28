package views

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The client views group the fleet by client ("geth", "lighthouse"): the
// team label of the managed containers. A validator client container carries
// the team of its consensus client. The layer comes from the kinds: cl or vc
// make a consensus client.
//
// A client is one row on the event stream (tableClients): its images, its
// policy and how many of its hosts are in which state. The hosts themselves
// are not on the stream a second time. GET /api/v1/clients/{client}/hosts
// serves them one page at a time, filtered on the controller, and the row's
// Version tells the page when a list it shows is out of date.

// ClientLayer says which row of the page a client belongs to.
type ClientLayer string

// Client layers.
const (
	LayerConsensus ClientLayer = "consensus"
	LayerExecution ClientLayer = "execution"
)

// ClientHostState is the one state a host has on a client's page. Where
// several apply the first of this list is shown.
type ClientHostState string

// Client host states, in their order of precedence.
const (
	// ClientFailed: the host's last lease failed, it is marked broken for an
	// image of the client, or a container of the client is down that should
	// be up.
	ClientFailed ClientHostState = "failed"
	// ClientRolling: the host holds a lease.
	ClientRolling ClientHostState = "rolling"
	// ClientHeld: a hold is in effect or wanted.
	ClientHeld ClientHostState = "held"
	// ClientOverridden: a container of the client has an override.
	ClientOverridden ClientHostState = "overridden"
	// ClientPending: a container of the client is behind its tag or has an
	// open need.
	ClientPending ClientHostState = "pending"
	// ClientCurrent: nothing of the above.
	ClientCurrent ClientHostState = "current"
)

// ClientHostStates lists the states in their order of precedence.
func ClientHostStates() []ClientHostState {
	return []ClientHostState{
		ClientFailed, ClientRolling, ClientHeld, ClientOverridden,
		ClientPending, ClientCurrent,
	}
}

// ClientImage is one image reference the containers of a client follow.
type ClientImage struct {
	Kind  protocol.ContainerKind `json:"kind"`
	Image string                 `json:"image"`
	// Digest and Revision are what the tag points at, by the freshest
	// registry check of the hosts on it. Empty when no host has looked yet.
	Digest   string `json:"digest,omitempty"`
	Revision string `json:"revision,omitempty"`
	// Hosts is how many hosts have a container on the reference, Behind how
	// many of them run another digest than Digest.
	Hosts  int `json:"hosts"`
	Behind int `json:"behind"`
	// Command is the command line most of these containers report without a
	// flag override: the client's configured flags for the kind.
	// CommandHosts is how many hosts report exactly it; the others on the
	// reference, without a flag override, run another one.
	Command      []string `json:"command,omitempty"`
	CommandHosts int      `json:"commandHosts"`
	// CommonArgs are the arguments more than half of those hosts report, in
	// the order they first appear. An argument only some hosts run, or one
	// that differs on every host, is not in it.
	CommonArgs []string `json:"commonArgs,omitempty"`
}

// ClientView is one client: a row of GET /api/v1/clients and of the stream.
type ClientView struct {
	Name  string      `json:"name"`
	Layer ClientLayer `json:"layer"`
	// Policies are the client's own image policies per kind of its images
	// (policyClients); a kind without one is not listed. DefaultPolicies is
	// what the image rollouts of each kind of the client answer to without
	// one (fleet.Settings.KindPolicy).
	Policies        map[protocol.ContainerKind]fleet.Policy `json:"policies"`
	DefaultPolicies map[protocol.ContainerKind]fleet.Policy `json:"defaultPolicies"`
	// Images are the configured image references of the client's
	// containers, by kind and reference.
	Images []ClientImage `json:"images"`

	// Hosts is the number of hosts that run the client. The others count the
	// hosts by their state (ClientHostState); they add up to Hosts.
	Hosts      int `json:"hosts"`
	Current    int `json:"current"`
	Pending    int `json:"pending"`
	Rolling    int `json:"rolling"`
	Overridden int `json:"overridden"`
	Held       int `json:"held"`
	Failed     int `json:"failed"`

	// ImageOverrides and FlagOverrides count the hosts on which a container
	// of the client has an image override, a flag override, whatever their
	// state.
	ImageOverrides int `json:"imageOverrides"`
	FlagOverrides  int `json:"flagOverrides"`

	// Version changes whenever anything in the client's host list changes.
	Version string `json:"version"`
}

// Clients is the body of GET /api/v1/clients.
type Clients struct {
	Clients []ClientView `json:"clients"`
}

// ClientHost is one host on a client's page.
type ClientHost struct {
	Host  string          `json:"host"`
	State ClientHostState `json:"state"`
	// Reason says more about the state: why the host waits, what failed,
	// why it is held.
	Reason string `json:"reason,omitempty"`
	// Stale: the agent is silent; everything else is its last report.
	Stale bool                `json:"stale,omitempty"`
	Slot  scheduler.SlotState `json:"slot"`
	// Containers are the host's containers of this client.
	Containers []protocol.Container `json:"containers"`
	// Usage is the host's resource use, for a login with the full view of
	// the host.
	Usage *HostUsage `json:"usage,omitempty"`
}

// ClientHosts is the body of GET /api/v1/clients/{client}/hosts.
type ClientHosts struct {
	Client string `json:"client"`
	// Version is ClientView.Version of the list the page was cut from.
	Version string `json:"version"`
	// Total is the number of hosts of the client, Matched how many of them
	// the filter selects; Hosts is the page of those from Offset on.
	Total   int          `json:"total"`
	Matched int          `json:"matched"`
	Offset  int          `json:"offset"`
	Limit   int          `json:"limit"`
	Hosts   []ClientHost `json:"hosts"`
	// Names, asked for with names=true instead of a page, are the names of
	// every matched host (at most maxActionHosts): what a selection of the
	// whole filter acts on.
	Names []string `json:"names,omitempty"`
}

// Clients is every client with its hosts, built from the host table.
type ClientIndex struct {
	// Views are the clients, consensus layer first, then by name.
	Views []ClientView
	// Hosts are the hosts of each client, by name.
	Hosts map[string][]ClientHost
}

// NewClientIndex groups the hosts by the clients of their managed
// containers. It is linear in the containers of the fleet.
func NewClientIndex(hosts []HostSummary, settings *fleet.Settings) *ClientIndex {
	idx := &ClientIndex{Hosts: make(map[string][]ClientHost, 8)}
	layers := make(map[string]ClientLayer, 8)

	for i := range hosts {
		sum := &hosts[i]

		for _, name := range hostClients(sum) {
			row := newClientHost(sum, name)
			idx.Hosts[name] = append(idx.Hosts[name], row)

			for j := range row.Containers {
				if k := row.Containers[j].Kind; k == protocol.KindCL || k == protocol.KindVC {
					layers[name] = LayerConsensus
				}
			}
		}
	}

	idx.Views = make([]ClientView, 0, len(idx.Hosts))

	for name, rows := range idx.Hosts {
		slices.SortFunc(rows, func(a, b ClientHost) int { return cmp.Compare(a.Host, b.Host) })

		view := ClientView{
			Name:  name,
			Layer: cmp.Or(layers[name], LayerExecution),
			Hosts: len(rows),
		}
		view.count(rows)
		view.Images = clientImages(rows)
		view.Policies = make(map[protocol.ContainerKind]fleet.Policy, len(view.Images))
		view.DefaultPolicies = make(map[protocol.ContainerKind]fleet.Policy, len(view.Images))

		for i := range view.Images {
			kind := view.Images[i].Kind
			view.DefaultPolicies[kind], _ = settings.KindPolicy(kind)

			if p, ok := settings.PolicyClients.Get(kind, name); ok {
				view.Policies[kind] = p
			}
		}

		view.Version = clientVersion(rows)

		idx.Views = append(idx.Views, view)
	}

	slices.SortFunc(idx.Views, func(a, b ClientView) int {
		return cmp.Or(cmp.Compare(a.Layer, b.Layer), cmp.Compare(a.Name, b.Name))
	})

	return idx
}

// hostClients names the clients (teams) of a host's managed containers,
// sorted.
func hostClients(sum *HostSummary) []string {
	var names []string

	for i := range sum.Containers {
		c := &sum.Containers[i]
		if c.Managed && c.Team != "" && !slices.Contains(names, c.Team) {
			names = append(names, c.Team)
		}
	}

	slices.Sort(names)

	return names
}

// newClientHost is the row of one host on the page of one client.
func newClientHost(sum *HostSummary, client string) ClientHost {
	row := ClientHost{
		Host: sum.Name, Stale: sum.Stale, Slot: sum.Plan.Slot, Usage: NewHostUsage(sum.Resources),
	}

	for i := range sum.Containers {
		if c := &sum.Containers[i]; c.Managed && c.Team == client {
			row.Containers = append(row.Containers, *c)
		}
	}

	row.State, row.Reason = clientHostState(sum, row.Containers)

	return row
}

// clientHostState is the state of a host on a client's page, and what there
// is to say about it.
func clientHostState(
	sum *HostSummary, containers []protocol.Container,
) (state ClientHostState, reason string) {
	if reason := clientFailure(sum, containers); reason != "" {
		return ClientFailed, reason
	}

	if sum.Plan.Slot == scheduler.SlotLeased || sum.Plan.Slot == scheduler.SlotVerifying {
		return ClientRolling, string(sum.Plan.Slot)
	}

	switch {
	case sum.Hold.Local:
		return ClientHeld, cmp.Or(sum.Hold.LocalReason, "held on the host")
	case sum.Hold.Active():
		return ClientHeld, fleet.DescribeHolds(sum.Hold.Holds)
	}

	for i := range containers {
		if containers[i].Override != nil {
			return ClientOverridden, sum.Plan.Reason
		}
	}

	if clientPending(sum, containers) {
		return ClientPending, sum.Plan.Reason
	}

	return ClientCurrent, ""
}

// clientFailure says what is broken about the client on the host; empty when
// nothing is.
func clientFailure(sum *HostSummary, containers []protocol.Container) string {
	for i := range containers {
		c := &containers[i]

		if _, broken := sum.Broken[scheduler.ContainerUnitName(c)]; broken {
			return "the last lease for " + c.Image + " failed"
		}

		if c.State != protocol.StateRunning && c.ShouldRun() {
			return "container " + c.Name + " is " + c.State
		}
	}

	if sum.Plan.Slot == scheduler.SlotFailed {
		return cmp.Or(sum.Plan.Reason, "the last lease failed")
	}

	return ""
}

// clientPending reports whether a container of the client has an open need
// or runs another digest than its tag points at.
func clientPending(sum *HostSummary, containers []protocol.Container) bool {
	for i := range containers {
		c := &containers[i]

		if reg := c.Registry; reg != nil && reg.Digest != "" && reg.Digest != c.Digest {
			return true
		}

		for j := range sum.Needs {
			if sum.Needs[j].Container() == c.Name {
				return true
			}
		}
	}

	return false
}

// OverrideMatches reports whether the row passes the override filter: "image"
// or "flags" for the hosts with that part of an override, empty for all.
func OverrideMatches(row *ClientHost, part string) bool {
	image, flags := overrideParts(row)

	switch part {
	case protocol.OverrideImage:
		return image
	case protocol.OverrideFlags:
		return flags
	default:
		return true
	}
}

// overrideParts reports whether a container of the row has an image
// override, a flag override.
func overrideParts(row *ClientHost) (image, flags bool) {
	for i := range row.Containers {
		if o := row.Containers[i].Override; o != nil {
			image = image || o.Image != ""
			flags = flags || len(o.AddFlags)+len(o.RemoveFlags) > 0
		}
	}

	return image, flags
}

func (v *ClientView) count(rows []ClientHost) {
	for i := range rows {
		image, flags := overrideParts(&rows[i])
		if image {
			v.ImageOverrides++
		}

		if flags {
			v.FlagOverrides++
		}

		switch rows[i].State {
		case ClientFailed:
			v.Failed++
		case ClientRolling:
			v.Rolling++
		case ClientHeld:
			v.Held++
		case ClientOverridden:
			v.Overridden++
		case ClientPending:
			v.Pending++
		default:
			v.Current++
		}
	}
}

// clientImages lists the configured image references of a client's
// containers with what their tags point at.
func clientImages(rows []ClientHost) []ClientImage {
	type key struct {
		kind  protocol.ContainerKind
		image string
	}

	type group struct {
		ClientImage

		checked  time.Time
		digests  []string
		commands map[string]int
		args     argVotes
	}

	groups := make(map[key]*group, 2)

	for i := range rows {
		seen := make(map[key]bool, 2)

		for j := range rows[i].Containers {
			c := &rows[i].Containers[j]

			k := key{kind: c.Kind, image: cmp.Or(c.ConfiguredImage, c.Image)}

			g := groups[k]
			if g == nil {
				g = &group{
					ClientImage: ClientImage{Kind: k.kind, Image: k.image},
					commands:    make(map[string]int, 4),
					args:        argVotes{count: make(map[string]int, 32)},
				}
				groups[k] = g
			}

			if !seen[k] {
				seen[k] = true
				g.Hosts++

				if o := c.Override; o == nil || len(o.AddFlags)+len(o.RemoveFlags) == 0 {
					g.commands[strings.Join(c.Command, "\x00")]++
					g.args.add(c.Command)
				}

				// A container on another reference (overridden, or moving to agent.yaml's) says
				// nothing about this tag.
				if c.Image == k.image {
					g.digests = append(g.digests, c.Digest)
				}
			}

			reg := c.Registry
			if c.Image != k.image || reg == nil || reg.Digest == "" {
				continue
			}

			if g.Digest == "" || reg.CheckedAt.After(g.checked) ||
				(reg.CheckedAt.Equal(g.checked) && reg.Digest < g.Digest) {
				g.Digest, g.Revision, g.checked = reg.Digest, reg.Revision, reg.CheckedAt
			}
		}
	}

	out := make([]ClientImage, 0, len(groups))

	for _, g := range groups {
		g.Command, g.CommandHosts = commonCommand(g.commands)
		g.CommonArgs = g.args.common()

		if g.Digest != "" {
			for _, d := range g.digests {
				if d != g.Digest {
					g.Behind++
				}
			}
		}

		out = append(out, g.ClientImage)
	}

	slices.SortFunc(out, func(a, b ClientImage) int {
		return cmp.Or(
			cmp.Compare(kindOrder(a.Kind), kindOrder(b.Kind)), cmp.Compare(a.Image, b.Image))
	})

	return out
}

// argVotes counts, per argument, the hosts that report it.
type argVotes struct {
	count map[string]int
	// order is the order the arguments were first seen.
	order []string
	hosts int
}

// add counts one host's command line; an argument it repeats counts once.
func (v *argVotes) add(command []string) {
	v.hosts++

	seen := make(map[string]struct{}, len(command))

	for _, arg := range command {
		if _, dup := seen[arg]; dup {
			continue
		}

		seen[arg] = struct{}{}

		if v.count[arg] == 0 {
			v.order = append(v.order, arg)
		}

		v.count[arg]++
	}
}

// common returns the arguments more than half of the hosts report.
func (v *argVotes) common() []string {
	out := make([]string, 0, len(v.order))

	for _, arg := range v.order {
		if v.count[arg]*2 > v.hosts {
			out = append(out, arg)
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// commonCommand is the command line most hosts report, of the counts by
// command line joined with NUL; on a tie the first by order.
func commonCommand(counts map[string]int) (command []string, hosts int) {
	best := ""

	for joined, n := range counts {
		if n > hosts || (n == hosts && joined < best) {
			best, hosts = joined, n
		}
	}

	if best == "" {
		return nil, hosts
	}

	return strings.Split(best, "\x00"), hosts
}

func kindOrder(k protocol.ContainerKind) int {
	return slices.Index([]protocol.ContainerKind{protocol.KindEL, protocol.KindCL, protocol.KindVC}, k)
}

// FleetKinds are the kinds of the managed containers the hosts report: el,
// cl and vc first, then by name. Unlabelled containers add none.
func FleetKinds(hosts []*fleet.Host) []protocol.ContainerKind {
	kinds := make([]protocol.ContainerKind, 0, 8)

	for _, host := range hosts {
		if host.Status == nil {
			continue
		}

		for i := range host.Status.Containers {
			c := &host.Status.Containers[i]
			if c.Managed && c.Kind != "" && !slices.Contains(kinds, c.Kind) {
				kinds = append(kinds, c.Kind)
			}
		}
	}

	rank := func(k protocol.ContainerKind) int {
		if i := kindOrder(k); i >= 0 {
			return i
		}

		return 3
	}

	slices.SortFunc(kinds, func(a, b protocol.ContainerKind) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b))
	})

	return kinds
}

// clientVersion is a digest of a client's host list: it differs whenever a
// page of the list would.
func clientVersion(rows []ClientHost) string {
	h := sha256.New()
	enc := json.NewEncoder(h)

	for i := range rows {
		row := rows[i]
		row.Containers = slices.Clone(row.Containers)

		// What moves with every sync without saying anything new.
		row.Usage = nil

		for j := range row.Containers {
			row.Containers[j].Stats = nil

			if reg := row.Containers[j].Registry; reg != nil {
				c := *reg
				c.CheckedAt = time.Time{}
				row.Containers[j].Registry = &c
			}
		}

		// Encoding a row of strings, numbers and times cannot fail, and the
		// hash never does.
		_ = enc.Encode(&row)
	}

	return hex.EncodeToString(h.Sum(nil)[:8])
}

// Clients builds the client views from the store.
func (v *Views) Clients(ctx context.Context) (*ClientIndex, error) {
	hosts, err := v.opts.Store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}

	settings, err := v.opts.Scheduler.Settings(ctx)
	if err != nil {
		return nil, err
	}

	plan := v.Plan()
	now := v.now()
	sums := make([]HostSummary, 0, len(hosts))

	for _, host := range hosts {
		sums = append(sums, Summarize(host, &plan, now, settings.HostStaleAfter.Std()))
	}

	return NewClientIndex(sums, &settings), nil
}
