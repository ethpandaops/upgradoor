// Package local is the agent's local socket: the server the running agent
// opens and the client the 'upgradoor host' commands use.
//
// The local socket is how a person on the host talks to the running agent.
// It is a Unix socket for root and one group (the entry user's), and it can
// do what that person could do anyway with SSH and the overrides file, plus
// ask for a roll: list the managed containers, set and clear an override (the
// agent writes the file), set and release the local hold, request a roll and
// follow it.
//
// Nothing that comes through the socket acts by itself. An override and a
// roll become needs, served under a lease of the controller like any other,
// within the verbs the configuration allows and never under a hold.
package local

import (
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Paths of the local API.
const (
	PathStatus    = "/v1/status"
	PathOverrides = "/v1/overrides"
	PathHold      = "/v1/hold"
	PathRolls     = "/v1/rolls"
	PathInit      = "/v1/init-overrides"
)

// Service is what the socket asks of the agent. user is the Unix user at the
// other end of the socket, as "local:<user>". An error that wraps
// auditlog.ErrRefused is the host's own rules refusing the request.
type Service interface {
	// Status returns what the agent last saw of the host.
	Status() Status
	// Override sets or clears (req.Clear) the override of one container and
	// returns what was set, as the history line describes it.
	Override(user string, req *OverrideRequest) (string, error)
	// Hold sets or releases (req.Release) the local hold.
	Hold(user string, req *HoldRequest) error
	// Roll requests a roll of one container.
	Roll(user, container string) (*Roll, error)
	// InitOverrides writes the generated block of the overrides file and
	// returns the file.
	InitOverrides(user string) (string, error)
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
	// OverridesFile is the overrides file; OverridesError says why it cannot
	// be read.
	OverridesFile  string `json:"overridesFile"`
	OverridesError string `json:"overridesError,omitempty"`
	// SyncError is set while the controller cannot be reached.
	SyncError string `json:"syncError,omitempty"`
}

// Container is one managed container in the local status.
type Container struct {
	protocol.Container

	// BaseCommand is the command line without the override's flags, when an
	// override changed it.
	BaseCommand []string `json:"baseCommand,omitempty"`
	// Target is the image the container is to run, when that is not the one
	// it runs.
	Target string `json:"target,omitempty"`
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
	// Detail says more: the controller's waiting reason, the error.
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
	// RollRecreated: the container is recreated; the agent watches that it
	// stays up.
	RollRecreated = "recreated"
	// RollVerified: the agent's check passed; the controller checks that the
	// node is back before it gives the slot away.
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
	// Until is when the override ends; absent for one without an end.
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

// Answer is the answer to every local request that changes something.
type Answer struct {
	Message string `json:"message,omitempty"`
	// Roll is the ID of a requested roll.
	Roll  string `json:"roll,omitempty"`
	Error string `json:"error,omitempty"`
}
