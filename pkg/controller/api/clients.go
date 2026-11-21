package api

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The client views group the fleet by client software ("geth",
// "lighthouse"): what a managed container's agent configuration names as its
// client. A validator client container belongs to the client of its
// consensus client.
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

// clientHostStates lists the states in their order of precedence.
func clientHostStates() []ClientHostState {
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
}

// ClientView is one client: a row of GET /api/v1/clients and of the stream.
type ClientView struct {
	Name  string      `json:"name"`
	Layer ClientLayer `json:"layer"`
	// Policy is the image policy the client's rollouts answer to; PolicySet
	// is false when that is the global default (policyImage).
	Policy    fleet.Policy `json:"policy"`
	PolicySet bool         `json:"policySet"`
	// Owners are the teams that own a container of the client on any host;
	// AllOwned names the teams that own every one of them, which is what the
	// team feature for the client's policy asks for.
	Owners   []string `json:"owners"`
	AllOwned []string `json:"allOwned"`
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

const (
	defaultClientHostLimit = 50
	maxClientHostLimit     = 200
	maxClientHostQuery     = 128
)

// clientIndex is every client with its hosts, built from the host table.
type clientIndex struct {
	views []ClientView
	hosts map[string][]ClientHost
}

// newClientIndex groups the hosts by the clients of their managed
// containers. It is linear in the containers of the fleet.
func newClientIndex(hosts []HostSummary, settings *fleet.Settings) *clientIndex {
	idx := &clientIndex{hosts: make(map[string][]ClientHost, 8)}
	layers := make(map[string]ClientLayer, 8)

	for i := range hosts {
		sum := &hosts[i]

		for _, name := range hostClients(sum) {
			row := newClientHost(sum, name)
			idx.hosts[name] = append(idx.hosts[name], row)

			for j := range row.Containers {
				if k := row.Containers[j].Kind; k == protocol.KindCL || k == protocol.KindVC {
					layers[name] = LayerConsensus
				}
			}
		}
	}

	idx.views = make([]ClientView, 0, len(idx.hosts))

	for name, rows := range idx.hosts {
		slices.SortFunc(rows, func(a, b ClientHost) int { return cmp.Compare(a.Host, b.Host) })

		view := ClientView{
			Name:   name,
			Layer:  cmp.Or(layers[name], LayerExecution),
			Policy: settings.ImagePolicy(name),
			Hosts:  len(rows),
		}
		_, view.PolicySet = settings.PolicyClients[name]

		view.count(rows)
		view.Images = clientImages(rows)
		view.Owners, view.AllOwned = clientOwners(rows)
		view.Version = clientVersion(rows)

		idx.views = append(idx.views, view)
	}

	slices.SortFunc(idx.views, func(a, b ClientView) int {
		return cmp.Or(cmp.Compare(a.Layer, b.Layer), cmp.Compare(a.Name, b.Name))
	})

	return idx
}

// hostClients names the clients of a host's managed containers, sorted.
func hostClients(sum *HostSummary) []string {
	var names []string

	for i := range sum.Containers {
		c := &sum.Containers[i]
		if c.Managed && c.Client != "" && !slices.Contains(names, c.Client) {
			names = append(names, c.Client)
		}
	}

	slices.Sort(names)

	return names
}

// newClientHost is the row of one host on the page of one client.
func newClientHost(sum *HostSummary, client string) ClientHost {
	row := ClientHost{Host: sum.Name, Stale: sum.Stale, Slot: sum.Plan.Slot}

	for i := range sum.Containers {
		if c := &sum.Containers[i]; c.Managed && c.Client == client {
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
	case sum.HoldWanted || sum.Hold.Controller:
		return ClientHeld, sum.HoldReason
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

		if _, broken := sum.Broken["image:"+c.Image]; broken {
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

// overrideMatches reports whether the row passes the override filter: "image"
// or "flags" for the hosts with that part of an override, empty for all.
func overrideMatches(row *ClientHost, part string) bool {
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
	}

	groups := make(map[key]*group, 2)

	for i := range rows {
		seen := make(map[key]bool, 2)

		for j := range rows[i].Containers {
			c := &rows[i].Containers[j]

			k := key{kind: c.Kind, image: cmp.Or(c.ConfiguredImage, c.Image)}

			g := groups[k]
			if g == nil {
				g = &group{ClientImage: ClientImage{Kind: k.kind, Image: k.image}, commands: map[string]int{}}
				groups[k] = g
			}

			if !seen[k] {
				seen[k] = true
				g.Hosts++

				if o := c.Override; o == nil || len(o.AddFlags)+len(o.RemoveFlags) == 0 {
					g.commands[strings.Join(c.Command, "\x00")]++
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

// clientOwners returns the teams that own a container of the client, and
// those among them that own every container of it.
func clientOwners(rows []ClientHost) (some, all []string) {
	some = []string{}
	first := true

	for i := range rows {
		for j := range rows[i].Containers {
			owners := rows[i].Containers[j].Owners

			for _, o := range owners {
				if !slices.Contains(some, o) {
					some = append(some, o)
				}
			}

			if first {
				all, first = slices.Clone(owners), false

				continue
			}

			all = slices.DeleteFunc(all, func(o string) bool { return !slices.Contains(owners, o) })
		}
	}

	slices.Sort(some)
	slices.Sort(all)

	return some, nonNil(all)
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
		for j := range row.Containers {
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

// clientIndex builds the client views from the store.
func (s *Server) clientIndex(r *http.Request) (*clientIndex, error) {
	ctx := r.Context()

	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		return nil, err
	}

	plan := s.opts.Scheduler.Plan()
	now := s.now()
	sums := make([]HostSummary, 0, len(hosts))

	for _, host := range hosts {
		sums = append(sums, hostSummary(host, plan, now, settings.HostStaleAfter.Std()))
	}

	return newClientIndex(sums, &settings), nil
}

// handleClients serves GET /api/v1/clients.
func (s *Server) handleClients(w http.ResponseWriter, r *http.Request, _ *user) {
	idx, err := s.clientIndex(r)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, Clients{Clients: idx.views})
}

// handleClientHosts serves
// GET /api/v1/clients/{client}/hosts?state=&q=&override=&offset=&limit=&names=:
// one page of the hosts that run the client, ordered by name. state is a list
// of ClientHostState separated by commas, q a part of the host name, override
// "image" or "flags" (hosts with that part of an override). names=true
// answers the names of every matched host instead of a page.
func (s *Server) handleClientHosts(w http.ResponseWriter, r *http.Request, _ *user) {
	query := r.URL.Query()
	needle := strings.ToLower(strings.TrimSpace(query.Get("q")))

	if len(needle) > maxClientHostQuery {
		s.writeError(w, http.StatusBadRequest, "q is too long")

		return
	}

	var states []ClientHostState

	for part := range strings.SplitSeq(query.Get("state"), ",") {
		switch state := ClientHostState(part); {
		case part == "":
		case slices.Contains(clientHostStates(), state):
			states = append(states, state)
		default:
			s.writeError(w, http.StatusBadRequest, "unknown state in the state filter")

			return
		}
	}

	override := query.Get("override")
	if override != "" && override != protocol.OverrideImage && override != protocol.OverrideFlags {
		s.writeError(w, http.StatusBadRequest, "override must be image or flags")

		return
	}

	names := query.Get("names") == "true"

	limit, err := queryLimit(r, defaultClientHostLimit, maxClientHostLimit)
	if err != nil {
		s.writeBadRequest(w, err)

		return
	}

	offset := 0

	if raw := query.Get("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			s.writeError(w, http.StatusBadRequest, "offset must not be negative")

			return
		}
	}

	idx, err := s.clientIndex(r)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	name := r.PathValue("client")

	at := slices.IndexFunc(idx.views, func(v ClientView) bool { return v.Name == name })
	if at < 0 {
		s.writeError(w, http.StatusNotFound, "unknown client")

		return
	}

	rows := idx.hosts[name]
	out := ClientHosts{
		Client: name, Version: idx.views[at].Version, Total: len(rows),
		Offset: offset, Limit: limit, Hosts: make([]ClientHost, 0, limit),
	}

	for i := range rows {
		row := &rows[i]

		if (len(states) > 0 && !slices.Contains(states, row.State)) ||
			(needle != "" && !strings.Contains(strings.ToLower(row.Host), needle)) ||
			!overrideMatches(row, override) {
			continue
		}

		switch {
		case names:
			if len(out.Names) < maxActionHosts {
				out.Names = append(out.Names, row.Host)
			}
		case out.Matched >= offset && len(out.Hosts) < limit:
			out.Hosts = append(out.Hosts, *row)
		}

		out.Matched++
	}

	s.writeJSON(w, http.StatusOK, out)
}
