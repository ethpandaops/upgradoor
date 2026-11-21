// Package api serves the controller's HTTP API: the agent endpoints, the
// public views and the authenticated views and actions.
//
// Routes (all JSON; errors are httpjson.ErrorBody with a non-2xx status):
//
//	POST /api/agent/v1/sync            agent, certificate + proof   protocol.SyncRequest -> protocol.SyncResponse
//	POST /api/agent/v1/inventory       agent, certificate + proof   protocol.Inventory -> protocol.InventoryResponse
//
//	GET  /api/v1/config                public   -> Config
//	GET  /api/v1/public/hosts          public   -> PublicHosts
//	GET  /api/v1/public/images         public   -> PublicImages
//	GET  /api/v1/public/network        public   -> scheduler.Gate
//	GET  /api/v1/public/slots?window=  public   -> SlotHistory, without hosts
//
//	GET  /api/v1/hosts                 user     -> Hosts
//	GET  /api/v1/hosts/{name}          user     -> HostDetail
//	GET  /api/v1/packages?q=&limit=    user     -> Packages
//	GET  /api/v1/rollouts?limit=       user     -> Rollouts
//	GET  /api/v1/plan                  user     -> scheduler.Plan
//	GET  /api/v1/settings              user     -> SettingsBody
//	PUT  /api/v1/settings              user     fleet.Settings -> SettingsBody
//	GET  /api/v1/audit?target=&action=&before=&limit=   user -> Audit
//	GET  /api/v1/slots?window=         user     -> SlotHistory
//	POST /api/v1/actions/roll          user     scheduler.RollRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/reboot        user     scheduler.RebootRequest       -> scheduler.ActionResult
//	POST /api/v1/actions/hold          user     scheduler.HoldRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/unhold        user     scheduler.HoldRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/mark-offline  user     scheduler.OfflineRequest      -> scheduler.ActionResult
//	POST /api/v1/actions/unmark-offline user    scheduler.OfflineRequest      -> scheduler.ActionResult
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
package views

import (
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

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

// HostSummary is one row of the authenticated host table.
type HostSummary struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
	// Validators is the number of validators counted for the host.
	Validators uint64 `json:"validators"`
	// ValidatorReport is what the host's last sync said about its validator
	// range: the range, where it was read from, and whether the host knows
	// it at all. Absent when the host reports none.
	ValidatorReport *protocol.Validators `json:"validatorReport,omitempty"`
	LastSeen        time.Time            `json:"lastSeen"`
	Stale           bool                 `json:"stale"`
	AgentVersion    string               `json:"agentVersion"`

	Containers    []protocol.Container `json:"containers"`
	Node          protocol.NodeHealth  `json:"node"`
	OS            protocol.OSSummary   `json:"os"`
	Pending       protocol.Pending     `json:"pending"`
	UptimeSeconds int64                `json:"uptimeSeconds"`
	Hold          protocol.Hold        `json:"hold"`
	HoldWanted    bool                 `json:"holdWanted"`
	// HoldReason is the reason of the controller hold (HoldWanted), as the
	// person gave it. It is for the authenticated views only.
	HoldReason string `json:"holdReason,omitempty"`
	// HoldBy says who set the controller hold: "team" for a hold set through
	// a team feature, which a team feature may lift again, "staff" for every
	// other hold. Absent when the host is not held.
	HoldBy string `json:"holdBy,omitempty"`
	// Offline is the host's known-offline mark while it is in force: its
	// validators are left out of the gate and the budget, and it gets no
	// lease. Absent otherwise.
	Offline *fleet.OfflineMark `json:"offline,omitempty"`
	// Broken says what the host's last lease failed on (fleet.Host.Broken):
	// per group of rollouts (scheduler.ImageGroup, "packages") the ID of the
	// lease that failed. Such a host goes first in the next rollout of the
	// group. Absent when the host is in order.
	Broken map[string]string `json:"broken,omitempty"`
	Verbs  []protocol.Verb   `json:"verbs"`
	Needs  []protocol.Need   `json:"needs"`

	// Plan is the scheduler's view: slot state, attesting validators and
	// waiting reason.
	Plan scheduler.HostPlan `json:"plan"`
}

// HostListing is one row of the host list: what the overview, the host
// table and the selection need of a host. The rest of a HostSummary (the
// container internals, the node health, the validator report, the needs in
// full, the broken marks) is in the host's details.
type HostListing struct {
	Name          string             `json:"name"`
	Roles         []string           `json:"roles"`
	Validators    uint64             `json:"validators"`
	LastSeen      time.Time          `json:"lastSeen"`
	Stale         bool               `json:"stale"`
	AgentVersion  string             `json:"agentVersion"`
	Containers    []ContainerListing `json:"containers"`
	OS            protocol.OSSummary `json:"os"`
	Pending       protocol.Pending   `json:"pending"`
	UptimeSeconds int64              `json:"uptimeSeconds"`
	Hold          protocol.Hold      `json:"hold"`
	HoldWanted    bool               `json:"holdWanted"`
	HoldReason    string             `json:"holdReason,omitempty"`
	HoldBy        string             `json:"holdBy,omitempty"`
	Offline       *fleet.OfflineMark `json:"offline,omitempty"`
	Verbs         []protocol.Verb    `json:"verbs"`
	// Needs are the kinds of the host's open needs, one entry per need.
	Needs []NeedListing      `json:"needs"`
	Plan  scheduler.HostPlan `json:"plan"`
}

// ContainerName is the name of a container in a listing. Names repeat from
// host to host, so a stream carries each once.
type ContainerName string

// ContainerListing is a container as the host list shows it.
type ContainerListing struct {
	Name     ContainerName          `json:"name"`
	Image    string                 `json:"image"`
	Digest   string                 `json:"digest,omitempty"`
	Revision string                 `json:"revision,omitempty"`
	State    string                 `json:"state"`
	Managed  bool                   `json:"managed"`
	Kind     protocol.ContainerKind `json:"kind,omitempty"`
	Impact   protocol.Impact        `json:"impact,omitempty"`
	Owners   []string               `json:"owners,omitempty"`
	// Group is the roll group of a managed container (DefaultGroup for one
	// that neither a label nor the name map puts in a group).
	Group string `json:"group,omitempty"`
}

// NeedListing is an open need as the host list shows it.
type NeedListing struct {
	Kind protocol.NeedKind `json:"kind"`
}

// Hosts is the body of GET /api/v1/hosts.
type Hosts struct {
	Hosts []HostListing `json:"hosts"`
}

// HostDetail is the body of GET /api/v1/hosts/{name}.
type HostDetail struct {
	HostSummary

	CertHash   string    `json:"certHash"`
	Generation uint64    `json:"generation"`
	FirstSeen  time.Time `json:"firstSeen"`
	RemoteAddr string    `json:"remoteAddr"`
	// ValidatorStart and ValidatorEnd are the [start, end) range counted for
	// the host. It follows the range the host reports (ValidatorReport) and
	// differs from it while the host does not know its range (the last one
	// is kept) and where another host holds validators of it.
	ValidatorStart *uint64 `json:"validatorStart,omitempty"`
	ValidatorEnd   *uint64 `json:"validatorEnd,omitempty"`
	// ValidatorOverlaps names the other hosts that report validators this
	// host reports too.
	ValidatorOverlaps []string `json:"validatorOverlaps,omitempty"`

	// Inventory is nil until the agent has uploaded one.
	Inventory *protocol.Inventory `json:"inventory,omitempty"`
	// Leases are the host's recent leases, newest first.
	Leases []*fleet.Lease `json:"leases"`
	// History are the audit entries about this host, newest first.
	History []fleet.AuditEntry `json:"history"`
}

// Packages is the body of GET /api/v1/packages.
type Packages struct {
	Query   string               `json:"query"`
	Matches []store.PackageMatch `json:"matches"`
	// Total is the number of package names the query selects (all of them
	// without a query). Only the first page has it.
	Total *int `json:"total,omitempty"`
	// Next continues after this page (the "after" parameter); absent on the
	// last page.
	Next string `json:"next,omitempty"`
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

// AuditHead is what an event stream says about the audit log. The log itself
// is paged and filtered through GET /api/v1/audit; the stream says that there
// is something new to ask for.
type AuditHead struct {
	// LatestID is the ID of the newest audit entry, 0 for an empty log.
	LatestID int64 `json:"latestId"`
}
