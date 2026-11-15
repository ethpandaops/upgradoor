// Package api serves the controller's HTTP API: the agent endpoints, the
// public views and the authenticated views and actions.
//
// Routes (all JSON; errors are ErrorBody with a non-2xx status):
//
//	POST /api/agent/v1/sync            agent, certificate + proof   protocol.SyncRequest -> protocol.SyncResponse
//	POST /api/agent/v1/inventory       agent, certificate + proof   protocol.Inventory -> protocol.InventoryResponse
//
//	GET  /api/v1/config                public   -> Config
//	GET  /api/v1/public/hosts          public   -> PublicHosts
//	GET  /api/v1/public/images         public   -> PublicImages
//	GET  /api/v1/public/network        public   -> scheduler.Gate
//
//	GET  /api/v1/hosts                 user     -> Hosts
//	GET  /api/v1/hosts/{name}          user     -> HostDetail
//	GET  /api/v1/packages?q=&limit=    user     -> Packages
//	GET  /api/v1/rollouts?limit=       user     -> Rollouts
//	GET  /api/v1/plan                  user     -> scheduler.Plan
//	GET  /api/v1/settings              user     -> SettingsBody
//	PUT  /api/v1/settings              user     fleet.Settings -> SettingsBody
//	GET  /api/v1/audit?target=&action=&before=&limit=   user -> Audit
//	POST /api/v1/actions/roll          user     scheduler.RollRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/reboot        user     scheduler.RebootRequest       -> scheduler.ActionResult
//	POST /api/v1/actions/hold          user     scheduler.HoldRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/unhold        user     scheduler.HoldRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/pause         user     scheduler.ScopeRequest        -> scheduler.ActionResult
//	POST /api/v1/actions/resume        user     scheduler.ScopeRequest        -> scheduler.ActionResult
//	POST /api/v1/actions/skip          user     scheduler.RolloutHostsRequest -> scheduler.ActionResult
//	POST /api/v1/actions/retry         user     scheduler.RolloutHostsRequest -> scheduler.ActionResult
//	POST /api/v1/actions/abort         user     scheduler.ScopeRequest        -> scheduler.ActionResult
//	POST /api/v1/actions/forget        user     scheduler.HoldRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/end-lease     user     scheduler.EndLeaseRequest     -> scheduler.ActionResult
//
//	GET  /metrics                      Prometheus
//	GET  /healthz
//
// "user" routes need a bearer token from the authenticatoor when
// --auth-provider-url is set and are open otherwise. Every such token may
// read; PUT and POST need a feature of this service in the token, or its
// team twin for targets a team of the token owns (authz.go, actions.go), and
// are answered 403 without it. The public routes are
// the only unauthenticated view of host data and must never carry package
// versions, kernels, OS details, addresses or anything else not listed in
// their types.
package api

import (
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// ErrorBody is the body of every error answer.
type ErrorBody struct {
	Error string `json:"error"`
}

// Config tells the web UI how to run.
type Config struct {
	Network string `json:"network"`
	Version string `json:"version"`
	// AuthProviderURL is the authenticatoor base URL; empty means the API is
	// open and the UI shows everything without a login.
	AuthProviderURL string `json:"authProviderUrl"`
	// AuthService is the name of this service in a token's "services" claim.
	// The UI asks the auth client for the features of this service to know
	// which buttons to offer.
	AuthService string `json:"authService"`
}

// PublicImageRef is the public description of one running image.
type PublicImageRef struct {
	// Image is the reference with its tag, e.g. "sigp/lighthouse:unstable".
	Image    string `json:"image"`
	Digest   string `json:"digest,omitempty"`
	Revision string `json:"revision,omitempty"`
	// Custom marks a container whose image was swapped by hand.
	Custom bool `json:"custom,omitempty"`
}

// PublicHost is everything an unauthenticated visitor may learn about a host.
type PublicHost struct {
	Name string          `json:"name"`
	EL   *PublicImageRef `json:"el,omitempty"`
	CL   *PublicImageRef `json:"cl,omitempty"`
	// UpdatePending is true when the host has any open need.
	UpdatePending bool `json:"updatePending"`
}

// PublicHosts is the body of GET /api/v1/public/hosts.
type PublicHosts struct {
	Hosts []PublicHost `json:"hosts"`
}

// PublicImage summarises one EL or CL image reference across the fleet.
type PublicImage struct {
	Image string                 `json:"image"`
	Kind  protocol.ContainerKind `json:"kind"`
	// Digest is the digest the tag points at, as last seen by any agent.
	Digest   string `json:"digest,omitempty"`
	Revision string `json:"revision,omitempty"`
	// HostsOn run Digest; HostsBehind run the same reference at another
	// digest.
	HostsOn     []string `json:"hostsOn"`
	HostsBehind []string `json:"hostsBehind"`
}

// PublicImages is the body of GET /api/v1/public/images.
type PublicImages struct {
	Images []PublicImage `json:"images"`
}

// ParticipationPoint is the attestation participation of one epoch across
// every tracked validator. Only the sum over all hosts is public.
type ParticipationPoint struct {
	Epoch uint64 `json:"epoch"`
	// Time is the end of the epoch.
	Time  time.Time `json:"time"`
	Live  uint64    `json:"live"`
	Total uint64    `json:"total"`
}

// PublicHistory is the body of GET /api/v1/public/history: the recent
// participation, oldest epoch first, and the threshold the gate holds it to.
type PublicHistory struct {
	Epochs                  []ParticipationPoint `json:"epochs"`
	MinParticipationPercent float64              `json:"minParticipationPercent"`
	EpochSeconds            float64              `json:"epochSeconds"`
}

// HostSummary is one row of the authenticated host table.
type HostSummary struct {
	Name         string    `json:"name"`
	Roles        []string  `json:"roles"`
	Validators   uint64    `json:"validators"`
	LastSeen     time.Time `json:"lastSeen"`
	Stale        bool      `json:"stale"`
	AgentVersion string    `json:"agentVersion"`

	Containers    []protocol.Container `json:"containers"`
	Node          protocol.NodeHealth  `json:"node"`
	OS            protocol.OSSummary   `json:"os"`
	Pending       protocol.Pending     `json:"pending"`
	UptimeSeconds int64                `json:"uptimeSeconds"`
	Hold          protocol.Hold        `json:"hold"`
	HoldWanted    bool                 `json:"holdWanted"`
	// HoldReason is the reason of the controller hold (HoldWanted): what the
	// person gave, or that a lease of the host was ended. It is for the
	// authenticated views only.
	HoldReason string `json:"holdReason,omitempty"`
	// HoldBy says who set the controller hold: "team" for a hold set through
	// a team feature, which a team feature may lift again, "staff" for every
	// other hold. Absent when the host is not held.
	HoldBy string          `json:"holdBy,omitempty"`
	Verbs  []protocol.Verb `json:"verbs"`
	Needs  []protocol.Need `json:"needs"`

	// Plan is the scheduler's view: slot state, attesting validators and
	// waiting reason.
	Plan scheduler.HostPlan `json:"plan"`
}

// Hosts is the body of GET /api/v1/hosts.
type Hosts struct {
	Hosts []HostSummary `json:"hosts"`
}

// HostDetail is the body of GET /api/v1/hosts/{name}.
type HostDetail struct {
	HostSummary

	CertHash   string    `json:"certHash"`
	Generation uint64    `json:"generation"`
	FirstSeen  time.Time `json:"firstSeen"`
	RemoteAddr string    `json:"remoteAddr"`
	// ValidatorRange is the [start, end) range from the certificate.
	ValidatorStart *uint64 `json:"validatorStart,omitempty"`
	ValidatorEnd   *uint64 `json:"validatorEnd,omitempty"`

	// Inventory is nil until the agent has uploaded one.
	Inventory *protocol.Inventory `json:"inventory,omitempty"`
	// Liveness is the host's recent per-epoch liveness, newest first.
	Liveness []fleet.Liveness `json:"liveness"`
	// Leases are the host's recent leases, newest first.
	Leases []*fleet.Lease `json:"leases"`
	// History are the audit entries about this host, newest first.
	History []fleet.AuditEntry `json:"history"`
}

// Packages is the body of GET /api/v1/packages.
type Packages struct {
	Query   string               `json:"query"`
	Matches []store.PackageMatch `json:"matches"`
}

// Rollouts is the body of GET /api/v1/rollouts.
type Rollouts struct {
	// Open are all non-terminal rollouts; Recent the latest terminal ones.
	Open   []*fleet.Rollout `json:"open"`
	Recent []*fleet.Rollout `json:"recent"`
	// Leases are the open leases; RecentLeases the latest closed ones.
	Leases       []*fleet.Lease `json:"leases"`
	RecentLeases []*fleet.Lease `json:"recentLeases"`
}

// SettingsBody is the body of GET and the answer of PUT /api/v1/settings.
type SettingsBody struct {
	Settings fleet.Settings `json:"settings"`
	Defaults fleet.Settings `json:"defaults"`
}

// Audit is the body of GET /api/v1/audit.
type Audit struct {
	Entries []fleet.AuditEntry `json:"entries"`
	// NextBefore is the "before" value for the next page; 0 at the end.
	NextBefore int64 `json:"nextBefore"`
}
