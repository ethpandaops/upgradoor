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
	// Roles are the host's roles from its configuration, at most MaxRoles.
	Roles []string `json:"roles,omitempty"`
	// Hold reports the hold state. While held the agent reports no needs and
	// refuses leases.
	Hold Hold `json:"hold"`

	Containers []Container `json:"containers"`
	Node       NodeHealth  `json:"node"`
	OS         OSSummary   `json:"os"`
	Pending    Pending     `json:"pending"`

	// InventoryHash identifies the agent's current inventory. The controller
	// answers WantInventory when it does not hold that inventory.
	InventoryHash string `json:"inventoryHash"`

	// Tags are the tags the host follows (references as it writes them, at
	// most MaxTags): those its managed containers run and those they are to
	// move to. The controller looks them up and answers Digests.
	Tags []string `json:"tags,omitempty"`

	// Needs are all currently open needs of the host.
	Needs []Need `json:"needs"`
	// Operation is the lease the agent is working on or has just finished.
	Operation *Operation `json:"operation,omitempty"`
	// DirectiveAcks answer directives received in earlier responses.
	DirectiveAcks []DirectiveAck `json:"directiveAcks,omitempty"`
	// Actions are the host's configured actions, sorted by name, at most
	// MaxActions.
	Actions []ActionInfo `json:"actions,omitempty"`
}

// Hold is the agent's hold state.
type Hold struct {
	// Local is true when the agent holds a local hold (or a file of the host
	// cannot be read). Only a person on the
	// host can lift it; the controller cannot.
	Local bool `json:"local"`
	// LocalReason is the reason of the local hold.
	LocalReason string `json:"localReason,omitempty"`
	// Controller is true when a hold directive from the controller is in
	// effect. A release-hold directive lifts it.
	Controller bool `json:"controller"`
}

// Active reports whether any hold is in effect.
func (h Hold) Active() bool { return h.Local || h.Controller }

// ContainerKind is what a managed container is, by its LabelKind label: a
// free-form name (ValidKind) such as el, cl, vc or snooper-el. An unlabelled
// container has none.
type ContainerKind string

// The kinds with a meaning of their own.
const (
	// KindEL, KindCL and KindVC are the node's clients: the EL and CL layers
	// of the clients page, the probes with their own checks, and the start
	// order el, cl, vc.
	KindEL ContainerKind = "el"
	KindCL ContainerKind = "cl"
	KindVC ContainerKind = "vc"
	// KindNone as the value of LabelKind leaves a container out of everything.
	KindNone ContainerKind = "none"
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
	// ImpactIndependent: the container touches neither the node nor the
	// network. Work on it has its own limit (Settings.MaxIndependentPercent),
	// no gate and no budget; it is ready when it ran through the verify
	// window.
	ImpactIndependent Impact = "independent"
)

// Valid reports whether the impact is one of the classes.
func (i Impact) Valid() bool {
	return i == ImpactNode || i == ImpactIndependent
}

// rank orders the classes: a lease that serves several is as heavy as the
// heaviest.
func (i Impact) rank() int {
	if i == ImpactIndependent {
		return 0
	}

	return 1
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

// The labels Ansible sets on a container: the only per-container
// configuration of upgradoor. The agent finds its containers by them; a
// recreate keeps a container's labels.
//
// A container with any kind but KindNone is managed with impact node. A
// container without LabelKind follows the host's defaultPolicy.
const (
	// LabelKind is the container's kind (ContainerKind).
	LabelKind = "io.ethpandaops.upgradoor.kind"
	// LabelTeam is the team that owns the container, the client name
	// ("geth"): it keys the client's image policy and the team features.
	// Optional; a container without it is staff's.
	LabelTeam = "io.ethpandaops.upgradoor.team"
)

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

	// Managed is true for every container the agent looks after: one with a
	// kind label other than KindNone, and an unlabelled one on a host whose
	// defaultPolicy is roll.
	Managed bool `json:"managed"`
	// Kind is the LabelKind label of a managed container; empty for an
	// unlabelled one.
	Kind ContainerKind `json:"kind,omitempty"`
	// Team is the LabelTeam label of a labelled managed container; empty
	// when it has none. The controller authorizes team features with it and
	// serves it to authenticated users only.
	Team string `json:"team,omitempty"`
	// NotRolled says why the agent rolls this managed container to nothing:
	// it is pinned to a digest or it has links. Its updates are still
	// reported. Absent for a container the agent rolls.
	NotRolled string `json:"notRolled,omitempty"`
	// Impact is node for a labelled container, independent for an
	// unlabelled one; absent for unmanaged containers.
	Impact Impact `json:"impact,omitempty"`
	// ConfiguredImage is the image Ansible deployed the container with: its
	// entry in the host's managed.yaml or, without one, the image it was
	// created with.
	ConfiguredImage string `json:"configuredImage,omitempty"`

	// Registry is the last registry check for Image; nil for unmanaged
	// containers.
	Registry *RegistryCheck `json:"registry,omitempty"`

	// Command is the command line of a managed container (the arguments
	// after the entrypoint), which is what a flag override changes. Like the
	// team it is served to authenticated users only.
	Command []string `json:"command,omitempty"`
	// Override is the entry of the host's overrides file for a managed
	// container, or what is left to undo of one that is gone. Absent when
	// the container runs what the agent configuration says.
	Override *ContainerOverride `json:"override,omitempty"`
	// Overridable: the image and the command line may be overridden, on a
	// host with overrides enabled, for a container with a team.
	Overridable bool `json:"overridable,omitempty"`

	// Base is what the container's own labels say it was before an override
	// changed it. It is not part of the wire format: the agent's container
	// runtime fills it for the agent.
	Base *OverrideBase `json:"-"`
	// StopTimeout is the stop grace period the container was created with;
	// zero when it names none. Not part of the wire format.
	StopTimeout time.Duration `json:"-"`
	// Linked is true for a container created with links (--link), which a
	// recreate cannot keep. Not part of the wire format.
	Linked bool `json:"-"`
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

// RegistryCheck is what the agent knows of a tag: the digest the controller
// looked up, and whether the image is local at it.
type RegistryCheck struct {
	// Digest is the manifest digest the tag points at (the index digest for
	// multi-arch tags).
	Digest string `json:"digest,omitempty"`
	// Revision is the revision label of the image for Digest, read from the
	// local image: it is empty until that image is pulled.
	Revision string `json:"revision,omitempty"`
	// CheckedAt is when the controller looked the tag up.
	CheckedAt time.Time `json:"checkedAt"`
	// Error is the lookup, local image or pull error; Digest then holds the
	// last good value.
	Error string `json:"error,omitempty"`
	// Pulled is true when the image for Digest is present locally.
	Pulled bool `json:"pulled"`
	// PullFailed is set after a lease could not pull the image, until the
	// agent lets it be tried again; Error says why.
	PullFailed bool `json:"pullFailed,omitempty"`
}

// NodeHealth is the result of the agent's local probes. A nil EL or CL
// means the probe is not configured.
type NodeHealth struct {
	EL *ELHealth `json:"el,omitempty"`
	CL *CLHealth `json:"cl,omitempty"`
	// Probes are the HTTP probes of the other kinds, sorted by kind.
	Probes []KindHealth `json:"probes,omitempty"`
}

// KindHealth is the HTTP probe of one kind: a GET that answers 2xx is
// healthy.
type KindHealth struct {
	Kind    ContainerKind `json:"kind"`
	Healthy bool          `json:"healthy"`
	Error   string        `json:"error,omitempty"`
}

// KindMissing says why the probe of a kind does not count as healthy; empty
// when it does or when the kind has no probe.
func (n NodeHealth) KindMissing(kind ContainerKind) string {
	for _, p := range n.Probes {
		if p.Kind != kind || p.Healthy {
			continue
		}

		if p.Error != "" {
			return "probe " + string(kind) + ": " + p.Error
		}

		return "probe " + string(kind) + " is not healthy"
	}

	return ""
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

	// Digests are what the tags of the request point at, by the controller's
	// lookups at the registries. The agent compares them with its local
	// images and pulls an image as the first phase of the lease that needs it.
	Digests []TagDigest `json:"digests,omitempty"`
}

// MaxTags bounds SyncRequest.Tags.
const MaxTags = 64

// MaxRoles bounds SyncRequest.Roles.
const MaxRoles = 32

// TagDigest is what one tag points at, by the controller's newest lookup.
type TagDigest struct {
	// Ref is the reference as the host wrote it.
	Ref string `json:"ref"`
	// Digest is the manifest digest (the index digest for multi-arch tags);
	// after a failed lookup the last good one, empty when there is none.
	Digest    string    `json:"digest,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	// Error is why the newest lookup failed.
	Error string `json:"error,omitempty"`
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
