package local

import (
	"context"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Paths of the socket API.
const (
	PathStatus    = "/v1/status"
	PathOverrides = "/v1/overrides"
	PathHold      = "/v1/hold"
	PathRolls     = "/v1/rolls"
	PathInit      = "/v1/init-overrides"
	PathDeploy    = "/v1/deploy"
)

// Agent is what the socket asks of the running agent; user is "local:<unix
// user>". An error wrapping plan.ErrRefused is answered 409, any other 500.
type Agent interface {
	// Status returns what the agent last saw of the host.
	Status() Status
	// Override sets or clears the override of one container and returns
	// what was set.
	Override(user string, req *OverrideRequest) (string, error)
	// Hold sets or releases the local hold.
	Hold(user string, req *HoldRequest) error
	// Roll requests a roll of one container.
	Roll(user, container string) (*Roll, error)
	// InitOverrides lays out the overrides file and returns its path.
	InitOverrides(user string) (string, error)
	// Deploy reads managed.yaml again after upgradoor deploy wrote a
	// container's entry; only root reaches it.
	Deploy(ctx context.Context, user string, req *DeployRequest) (*DeployResult, error)
}

// Status is the answer of GET /v1/status.
type Status struct {
	Host    string `json:"host"`
	Network string `json:"network"`
	Version string `json:"version"`
	// At is when the agent last looked at the host.
	At    time.Time       `json:"at"`
	Verbs []protocol.Verb `json:"verbs"`
	Hold  protocol.Hold   `json:"hold"`
	// Containers are the managed containers.
	Containers []Container     `json:"containers"`
	Needs      []protocol.Need `json:"needs"`
	// Queue is the controller's last word on why the host waits.
	Queue *protocol.Queue `json:"queue,omitempty"`
	// Lease and Operation are the lease the agent holds and its work on it.
	Lease     string              `json:"lease,omitempty"`
	Operation *protocol.Operation `json:"operation,omitempty"`
	// Rolls are the rolls requested through the socket, oldest first.
	Rolls []Roll `json:"rolls"`
	// OverridesFile is empty when overrides are disabled; OverridesError
	// says why it or managed.yaml cannot be read.
	OverridesFile  string `json:"overridesFile"`
	OverridesError string `json:"overridesError,omitempty"`
	// SyncError is set while the controller cannot be reached.
	SyncError string `json:"syncError,omitempty"`
}

// Container is one managed container in the local status.
type Container struct {
	protocol.Container

	// BaseCommand is the command line without the override's flags.
	BaseCommand []string `json:"baseCommand,omitempty"`
	// Target is the image the container is to run when not in sync.
	Target string `json:"target,omitempty"`
}

// ContainersOf is the managed containers of a view as the status lists them.
func ContainersOf(view *plan.View) []Container {
	out := make([]Container, 0, len(view.Containers))

	for i := range view.Containers {
		c := &view.Containers[i]
		if !c.Managed {
			continue
		}

		lc := Container{Container: *c}

		if t, ok := view.Targets[c.Name]; ok {
			if c.Base != nil && c.Base.HasCommand && !slices.Equal(c.Base.Command, c.Command) {
				lc.BaseCommand = slices.Clone(c.Base.Command)
			}

			if !t.InSync {
				lc.Target = t.Image
			}
		}

		out = append(out, lc)
	}

	return out
}

// Roll is one roll requested through the socket, and how far it got.
type Roll struct {
	ID          string    `json:"id"`
	Container   string    `json:"container"`
	RequestedBy string    `json:"requestedBy"`
	RequestedAt time.Time `json:"requestedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// State is one of the Roll* states.
	State string `json:"state"`
	// Detail is the controller's waiting reason or the error.
	Detail string `json:"detail,omitempty"`
	// Position is the host's place in the controller's queue while queued.
	Position int    `json:"position,omitempty"`
	Lease    string `json:"lease,omitempty"`
}

// States of a local roll. RollDone, RollFailed, RollRejected and
// RollSuperseded are final.
const (
	// RollResolving: the registry is asked and the image pulled.
	RollResolving = "resolving"
	// RollQueued: the need is reported; the host waits for a slot.
	RollQueued = "queued"
	// RollGranted: the controller granted a lease.
	RollGranted = "granted"
	// RollRecreating: the container is being recreated.
	RollRecreating = "recreating"
	// RollRecreated: recreated; the agent watches that it stays up.
	RollRecreated = "recreated"
	// RollVerified: the agent's check passed; the controller checks the node.
	RollVerified = "verified"
	// RollDone: the controller released the slot.
	RollDone = "done"
	// RollFailed: the roll failed; Detail says why.
	RollFailed = "failed"
	// RollRejected: the host's own rules refuse the roll.
	RollRejected = "rejected"
	// RollSuperseded: the roll was not needed any more, or was dropped.
	RollSuperseded = "superseded"
)

// Final reports whether the roll will not change any more.
func (r *Roll) Final() bool {
	return slices.Contains([]string{RollDone, RollFailed, RollRejected, RollSuperseded}, r.State)
}

// OverrideRequest is the body of POST /v1/overrides.
type OverrideRequest struct {
	Container   string   `json:"container"`
	Image       string   `json:"image,omitempty"`
	AddFlags    []string `json:"addFlags,omitempty"`
	RemoveFlags []string `json:"removeFlags,omitempty"`
	// Until is when the override ends; nil for no end.
	Until  *time.Time `json:"until,omitempty"`
	Reason string     `json:"reason,omitempty"`
	// Clear removes the override instead.
	Clear bool `json:"clear,omitempty"`
}

// HoldRequest is the body of POST /v1/hold.
type HoldRequest struct {
	Reason string `json:"reason"`
	// Release lifts the local hold instead.
	Release bool `json:"release,omitempty"`
}

// RollRequest is the body of POST /v1/rolls.
type RollRequest struct {
	Container string `json:"container"`
}

// DeployRequest is the body of POST /v1/deploy, sent after managed.yaml was
// written.
type DeployRequest struct {
	Container string `json:"container"`
	// Reset also clears the container's override.
	Reset bool `json:"reset,omitempty"`
}

// DeployResult is the answer of POST /v1/deploy, printed for Ansible.
type DeployResult struct {
	Container string `json:"container"`
	Exists    bool   `json:"exists"`
	// Managed: the agent rolls the container to its target.
	Managed bool `json:"managed"`
	// Changed: managed.yaml or the override changed.
	Changed bool `json:"changed"`
	// Roll is DeployRollRequested or DeployRollNone.
	Roll string `json:"roll"`
	// Ignore are the docker_container comparisons Ansible leaves to the agent.
	Ignore []string `json:"ignore"`
	// Pull: Ansible pulls the image itself.
	Pull bool `json:"pull"`
}

// Roll values of a DeployResult.
const (
	DeployRollRequested = "requested"
	DeployRollNone      = "none"
)

// Answer is the answer to every local request that changes something.
type Answer struct {
	Message string `json:"message,omitempty"`
	// Roll is the ID of a requested roll.
	Roll  string `json:"roll,omitempty"`
	Error string `json:"error,omitempty"`
}
