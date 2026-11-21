// Package protocol defines the wire types exchanged between the upgradoor
// agent and controller.
//
// The protocol is level-triggered: every sync restates the agent's complete
// current state, its open needs and its current operation; every response
// restates the host's lease and outstanding directives. A lost message or a
// restart on either side loses nothing.
//
// All bodies are JSON, optionally gzip-encoded (Content-Encoding: gzip). The
// request proof (pkg/identity) covers the body bytes as sent on the wire.
//
// The agents and the controller of a devnet are always the same build.
// Nothing here provides for a peer of another one: a field is what this build
// writes and reads, and a peer that speaks another Version is refused.
package protocol

import (
	"fmt"
	"time"
)

const (
	// Version is the protocol version spoken by this build.
	Version = 1

	// PathSync is the sync endpoint (POST).
	PathSync = "/api/agent/v1/sync"
	// PathInventory is the inventory endpoint (POST).
	PathInventory = "/api/agent/v1/inventory"

	// MaxSyncBody and MaxInventoryBody bound the accepted body sizes, before
	// and after decompression.
	MaxSyncBody      = 1 << 20
	MaxInventoryBody = 8 << 20
)

// Verb is one of the things an agent can be allowed to do. The set is closed.
type Verb string

// The closed verb set.
const (
	VerbRoll     Verb = "roll"
	VerbPackages Verb = "packages"
	VerbReboot   Verb = "reboot"
)

// SyncRequest is the body of POST /api/agent/v1/sync.
type SyncRequest struct {
	ProtocolVersion int       `json:"protocolVersion"`
	AgentVersion    string    `json:"agentVersion"`
	SentAt          time.Time `json:"sentAt"`

	// BootID is /proc/sys/kernel/random/boot_id; it changes on every boot.
	BootID        string `json:"bootId"`
	UptimeSeconds int64  `json:"uptimeSeconds"`

	// Verbs are the verbs the local configuration allows.
	Verbs []Verb `json:"verbs"`
	// Hold reports the hold state. While held the agent reports no needs and
	// refuses leases.
	Hold Hold `json:"hold"`

	Containers []Container `json:"containers"`
	// Validators is the host's validator range, read locally. Absent: the
	// host has no validators.
	Validators *Validators `json:"validators,omitempty"`
	Node       NodeHealth  `json:"node"`
	OS         OSSummary   `json:"os"`
	Pending    Pending     `json:"pending"`

	// InventoryHash identifies the agent's current inventory. The controller
	// answers WantInventory when it does not hold that inventory.
	InventoryHash string `json:"inventoryHash"`

	// Needs are all currently open needs of the host.
	Needs []Need `json:"needs"`
	// Operation is the lease the agent is working on or has just finished.
	Operation *Operation `json:"operation,omitempty"`
	// DirectiveAcks answer directives received in earlier responses.
	DirectiveAcks []DirectiveAck `json:"directiveAcks,omitempty"`
}

// Hold is the agent's hold state.
type Hold struct {
	// Local is true when /etc/upgradoor/hold exists. Only a person on the
	// host can lift it; the controller cannot.
	Local bool `json:"local"`
	// LocalReason is the first line of the hold file, if any.
	LocalReason string `json:"localReason,omitempty"`
	// Controller is true when a hold directive from the controller is in
	// effect. A release-hold directive lifts it.
	Controller bool `json:"controller"`
}

// Active reports whether any hold is in effect.
func (h Hold) Active() bool { return h.Local || h.Controller }

// ContainerKind classifies a managed container.
type ContainerKind string

// Container kinds.
const (
	KindEL    ContainerKind = "el"
	KindCL    ContainerKind = "cl"
	KindVC    ContainerKind = "vc"
	KindOther ContainerKind = "other"
)

// Impact says what restarting a managed container takes away, and so how
// the controller schedules work on it.
type Impact string

// Impact classes, from the heaviest to the lightest.
const (
	// ImpactNode: the container is part of the node. Work on it counts the
	// host's validators against the availability budget, takes a place in
	// the host limit, waits for the network gate, and is ready when the node
	// is back at head with peers and its validators attest.
	ImpactNode Impact = "node"
	// ImpactSlot: the container counts for the host limit and the network
	// gate, not for the validator budget; it is ready when it ran through the
	// verify window.
	ImpactSlot Impact = "slot"
	// ImpactIndependent: the container touches neither the node nor the
	// network. Work on it has its own limit (Settings.MaxIndependentPercent),
	// no gate and no budget; it is ready when it ran through the verify
	// window.
	ImpactIndependent Impact = "independent"
)

// DefaultImpact is the impact of a container of a kind that names none:
// independent for KindOther, node for the node's own containers.
func DefaultImpact(kind ContainerKind) Impact {
	if kind == KindOther {
		return ImpactIndependent
	}

	return ImpactNode
}

// Valid reports whether the impact is one of the classes.
func (i Impact) Valid() bool {
	return i == ImpactNode || i == ImpactSlot || i == ImpactIndependent
}

// rank orders the classes: a lease that serves several is as heavy as the
// heaviest.
func (i Impact) rank() int {
	switch i {
	case ImpactIndependent:
		return 0
	case ImpactSlot:
		return 1
	default:
		return 2
	}
}

// Heavier returns the heavier of two classes. The empty class counts as
// node.
func (i Impact) Heavier(o Impact) Impact {
	if i == "" || o == "" {
		return ImpactNode
	}

	if o.rank() > i.rank() {
		return o
	}

	return i
}

// Docker container states.
const (
	StateRunning    = "running"
	StateRestarting = "restarting"
	// StateMissing is the state the agent reports for a managed container
	// that does not exist. The agent never creates a container: the first
	// deployment is Ansible's.
	StateMissing = "missing"
	StateExited  = "exited"
	StateDead    = "dead"
)

// Container is one Docker container on the host.
type Container struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	// Image is the image reference the container was created from, as
	// written (e.g. "ethpandaops/lighthouse:unstable").
	Image string `json:"image"`
	// ImageID is the local image (config) ID.
	ImageID string `json:"imageId"`
	// Digest is the registry manifest digest of the running image for the
	// repository in Image (from the image's RepoDigests); empty for images
	// that were never pulled from or pushed to a registry.
	Digest string `json:"digest,omitempty"`
	// Revision is the org.opencontainers.image.revision label, Version the
	// org.opencontainers.image.version label of the running image.
	Revision string `json:"revision,omitempty"`
	Version  string `json:"version,omitempty"`

	State string `json:"state"`
	// ExitCode is the exit code of a container that is not running (exited
	// or dead); 0 otherwise.
	ExitCode     int       `json:"exitCode,omitempty"`
	Health       string    `json:"health,omitempty"`
	RestartCount int       `json:"restartCount"`
	StartedAt    time.Time `json:"startedAt"`

	// Managed is true for containers listed in the agent configuration.
	Managed bool          `json:"managed"`
	Kind    ContainerKind `json:"kind,omitempty"`
	// Impact is the container's impact class from the agent configuration;
	// absent for unmanaged containers.
	Impact Impact `json:"impact,omitempty"`
	// ConfiguredImage is the image reference from the agent configuration.
	ConfiguredImage string `json:"configuredImage,omitempty"`

	// Registry is the last registry check for Image; nil for unmanaged
	// containers.
	Registry *RegistryCheck `json:"registry,omitempty"`

	// Owners are the teams that own a managed container, from the agent
	// configuration: sorted, at most MaxOwners, each a team name (ValidOwner).
	// Absent for unmanaged containers and for containers nobody owns. The
	// controller authorizes the actions of team members with it and serves it
	// to authenticated users only.
	Owners []string `json:"owners,omitempty"`

	// Client is the client software a managed container runs ("geth",
	// "lighthouse"), from the agent configuration (ValidClient). A validator
	// client container carries the client of its consensus client. Absent for
	// containers that are not listed under a client.
	Client string `json:"client,omitempty"`

	// Command is the command line of a managed container (the arguments
	// after the entrypoint), which is what a flag override changes. Like the
	// owners it is served to authenticated users only.
	Command []string `json:"command,omitempty"`
	// Override is the entry of the host's overrides file for a managed
	// container, or what is left to undo of one that is gone. Absent when
	// the container runs what the agent configuration says.
	Override *ContainerOverride `json:"override,omitempty"`
	// Overridable says what the agent configuration lets the controller
	// override on this container. Absent when nothing.
	Overridable *Overridable `json:"overridable,omitempty"`

	// Base is what the container's own labels say it was before an override
	// changed it. It is not part of the wire format: the agent's container
	// runtime fills it for the agent.
	Base *OverrideBase `json:"-"`
}

// OverrideBase is a container's form before an override, as its labels
// record it.
type OverrideBase struct {
	// Image is the configured image reference of a container that was put
	// on an override image; empty when its image was not overridden.
	Image string
	// Command is the command line before a flag override, when HasCommand.
	Command    []string
	HasCommand bool
}

// ContainerOverride is the override of one managed container, as the host's
// overrides file states it. The host is its only source: the controller
// stores none and shows what the agents report.
type ContainerOverride struct {
	// Image is the image reference the container runs instead of the
	// configured one; empty when the image is not overridden.
	Image string `json:"image,omitempty"`
	// AddFlags and RemoveFlags change the container's command line.
	AddFlags    []string `json:"addFlags,omitempty"`
	RemoveFlags []string `json:"removeFlags,omitempty"`
	// By and Reason are who set the override and why, as the file says. For
	// an override set through the controller By is the user the controller
	// named; the host cannot verify it.
	By     string `json:"by,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Until is when the override ends; absent for one without an end.
	Until *time.Time `json:"until,omitempty"`
	// Applied is true when the container runs what the override asks for.
	Applied bool `json:"applied"`
	// Restoring is true when the entry is gone (cleared or expired) and the
	// container still runs it: it waits for a lease to be put back.
	Restoring bool `json:"restoring,omitempty"`
	// Error says why the override cannot be applied as written.
	Error string `json:"error,omitempty"`
}

// Overridable is what the agent configuration lets the controller override
// on a container.
type Overridable struct {
	// Images are the allowed image repository patterns; empty when image
	// overrides are off.
	Images []string `json:"images,omitempty"`
	// Flags is true when flag overrides are on.
	Flags bool `json:"flags,omitempty"`
}

// ShouldRun reports whether the container is meant to be running: it runs,
// Docker is restarting it, or it stopped by itself with an error. A
// container that was never started, exited cleanly or was stopped or killed
// by a person (exit codes 137 and 143, what "docker stop" and "docker kill"
// leave behind) is meant to be down, and the agent leaves it down.
//
// The agent starts exactly these containers again after an operation. That
// is what lets a newer image repair a container that crashed on a broken
// one, without ever starting a validator client somebody stopped on purpose.
func (c *Container) ShouldRun() bool {
	switch c.State {
	case StateRunning, StateRestarting:
		return true
	case StateExited, StateDead:
		return c.ExitCode != 0 && c.ExitCode != 137 && c.ExitCode != 143
	default:
		return false
	}
}

// RegistryCheck is the result of the agent's last look at a tag.
type RegistryCheck struct {
	// Digest is the manifest digest the tag points at (the index digest for
	// multi-arch tags).
	Digest string `json:"digest,omitempty"`
	// Revision is the revision label of the image for Digest, read from the
	// local image: it is empty until that image is pulled.
	Revision  string    `json:"revision,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	// Error is the last check error; Digest then holds the last good value.
	Error string `json:"error,omitempty"`
	// Pulled is true when the image for Digest is present locally.
	Pulled bool `json:"pulled"`
}

// NodeHealth is the result of the agent's local EL and CL probes. A nil
// member means the probe is not configured.
type NodeHealth struct {
	EL *ELHealth `json:"el,omitempty"`
	CL *CLHealth `json:"cl,omitempty"`
}

// Synced reports whether the node is at head with peers: every configured
// probe is reachable, not syncing (the consensus client also: its execution
// client is not offline) and has at least minPeers peers. With no probe
// configured it reports true.
func (n NodeHealth) Synced(minPeers int) bool {
	return n.Missing(minPeers) == ""
}

// Missing says in plain words what keeps the node from being at head with
// minPeers peers; empty when nothing does. Reachability is named first, then
// syncing, then peers.
func (n NodeHealth) Missing(minPeers int) string {
	el, cl := n.EL, n.CL

	switch {
	case el != nil && !el.Reachable:
		return "EL is not reachable"
	case cl != nil && !cl.Reachable:
		return "CL is not reachable"
	case el != nil && el.Syncing, cl != nil && cl.Syncing:
		return "node is syncing"
	case cl != nil && cl.ELOffline:
		return "CL reports its EL offline"
	case el != nil && el.Peers < minPeers:
		return fmt.Sprintf("waiting for peers: EL has %d of %d", el.Peers, minPeers)
	case cl != nil && cl.Peers < minPeers:
		return fmt.Sprintf("waiting for peers: CL has %d of %d", cl.Peers, minPeers)
	default:
		return ""
	}
}

// ELHealth is the execution client probe (eth_syncing, net_peerCount,
// eth_blockNumber).
type ELHealth struct {
	Reachable bool   `json:"reachable"`
	Syncing   bool   `json:"syncing"`
	Peers     int    `json:"peers"`
	Head      uint64 `json:"head"`
	Error     string `json:"error,omitempty"`
}

// CLHealth is the consensus client probe (/eth/v1/node/syncing,
// /eth/v1/node/peer_count).
type CLHealth struct {
	Reachable    bool   `json:"reachable"`
	Syncing      bool   `json:"syncing"`
	Optimistic   bool   `json:"optimistic"`
	ELOffline    bool   `json:"elOffline"`
	Peers        int    `json:"peers"`
	HeadSlot     uint64 `json:"headSlot"`
	SyncDistance uint64 `json:"syncDistance"`
	Error        string `json:"error,omitempty"`
}

// OSSummary is the part of the OS state carried in every sync. It is served
// to authenticated users only.
type OSSummary struct {
	ID            string `json:"id"`
	VersionID     string `json:"versionId"`
	PrettyName    string `json:"prettyName"`
	Arch          string `json:"arch"`
	RunningKernel string `json:"runningKernel"`
	// NewestKernel is the newest installed kernel release.
	NewestKernel string `json:"newestKernel"`
	// RebootRequired is true when /var/run/reboot-required exists.
	RebootRequired bool `json:"rebootRequired"`
	// RebootPackages is the content of /var/run/reboot-required.pkgs.
	RebootPackages []string `json:"rebootPackages,omitempty"`
}

// Pending counts upgradable packages.
type Pending struct {
	Upgradable int `json:"upgradable"`
	Security   int `json:"security"`
	Blocked    int `json:"blocked"`
}

// SyncResponse is the answer to a sync.
type SyncResponse struct {
	ServerTime time.Time `json:"serverTime"`
	// WantInventory asks the agent to POST its inventory.
	WantInventory bool `json:"wantInventory"`
	// Lease is the host's active lease, restated on every sync until the
	// controller releases it. When absent, an agent whose operation is
	// terminal forgets that operation.
	Lease *Lease `json:"lease,omitempty"`
	// Directives are all outstanding directives for the host.
	Directives []Directive `json:"directives,omitempty"`
	// Queue explains why the host is waiting; for logs and the host's local
	// status only.
	Queue *Queue `json:"queue,omitempty"`
	// Ended is the host's newest lease when it is over and the host holds
	// none: how it ended, for the host's local status only. The agent acts
	// on nothing of it.
	Ended *EndedLease `json:"ended,omitempty"`
	// NextSyncMillis is when the controller wants the next sync, in
	// milliseconds from now: paced by fleet size and controller load, short
	// while the host has something to do. The agent keeps it within its
	// local bounds.
	NextSyncMillis int64 `json:"nextSyncMillis"`
}

// EndedLease says how a lease ended.
type EndedLease struct {
	ID string `json:"id"`
	// State is released, failed or expired.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// Queue is a queued host's position and one-line waiting reason.
type Queue struct {
	Position int    `json:"position"`
	Reason   string `json:"reason"`
}

// ErrorResponse is the body of every non-2xx agent API answer.
type ErrorResponse struct {
	Error      string    `json:"error"`
	ServerTime time.Time `json:"serverTime"`
}
