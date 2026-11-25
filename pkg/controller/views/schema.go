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
//	GET  /api/v1/images?q=&limit=      user     -> Images
//	POST /api/v1/images/refresh        user     {} -> ImagesRefresh
//	GET  /api/v1/rollouts?limit=       user     -> Rollouts
//	GET  /api/v1/plan                  user     -> scheduler.Plan
//	GET  /api/v1/settings              admin    -> SettingsBody
//	PUT  /api/v1/settings              user     fleet.Settings -> SettingsBody
//	GET  /api/v1/settings/sources      admin    -> DataSources
//	POST /api/v1/settings/sources/validator-ranges/refresh  admin  {} -> ValidatorRangesRefresh
//	GET  /api/v1/audit?target=&action=&before=&limit=   user -> Audit
//	GET  /api/v1/slots?window=         user     -> SlotHistory
//	POST /api/v1/actions/roll          user     scheduler.RollRequest         -> scheduler.ActionResult
//	POST /api/v1/actions/reboot        user     scheduler.RebootRequest       -> scheduler.ActionResult
//	POST /api/v1/actions/host-action   user     scheduler.HostActionRequest   -> scheduler.ActionResult
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
//	POST /api/v1/actions/ignore-gate   user     scheduler.ScopeRequest        -> scheduler.ActionResult
//	POST /api/v1/actions/ignore-budget user     scheduler.ScopeRequest        -> scheduler.ActionResult
//	POST /api/v1/actions/approve-lease user     scheduler.ApproveLeaseRequest -> scheduler.ActionResult
//
//	GET  /metrics                      Prometheus
//	GET  /healthz
//
// "user" routes need a bearer token from the authenticatoor when
// --auth-provider-url is set and are open otherwise. Every such token may
// read but the "admin" routes; PUT and POST need a feature of this service
// in the token, or its team twin for targets a team of the token owns
// (authz.go, actions.go), and are answered 403 without it. What a login
// reads of the hosts depends on authz.FeatureDetails, of the audit log on
// authz.FeatureAdmin too (Access). No user view carries the error texts of
// the beacon service or the validator ranges inventory (PublicGate); the
// data sources do. The public routes are
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
	// UIScript is a script the UI includes, e.g. a site menu, with its
	// subresource integrity hash; absent when there is none.
	UIScript          string `json:"uiScript,omitempty"`
	UIScriptIntegrity string `json:"uiScriptIntegrity,omitempty"`
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
	// Validators is the number of the host's validators active on chain, as
	// the scheduler's plan counts them (HostPlan.Weight).
	Validators   uint64    `json:"validators"`
	LastSeen     time.Time `json:"lastSeen"`
	Stale        bool      `json:"stale"`
	AgentVersion string    `json:"agentVersion"`
	// AgentProtocol is the protocol version of the agent's last sync; zero
	// before the first.
	AgentProtocol int `json:"agentProtocol"`
	// Incompatible says why the controller serves the host nothing: its agent
	// speaks a protocol version the controller does not support. Absent when
	// it is served.
	Incompatible string `json:"incompatible,omitempty"`

	Containers    []protocol.Container `json:"containers"`
	Node          protocol.NodeHealth  `json:"node"`
	OS            protocol.OSSummary   `json:"os"`
	Pending       protocol.Pending     `json:"pending"`
	UptimeSeconds int64                `json:"uptimeSeconds"`
	// Resources is the host's resource use by its last sync; absent before
	// the first and for a login without the full view.
	Resources *protocol.Resources `json:"resources,omitempty"`
	// Hold is the hold state the agent reports: its holds with who set
	// each, why and when. The reasons are for the authenticated views only.
	// Plan.HoldRequests are the hold requests the agent has not answered.
	Hold protocol.Hold `json:"hold"`
	// Offline is the host's known-offline mark while it is in force: its
	// validators are left out of the gate and the budget, and it gets no
	// lease. Absent otherwise.
	Offline *fleet.OfflineMark `json:"offline,omitempty"`
	// Broken says what the host's last lease failed on (fleet.Host.Broken):
	// per group of rollouts (scheduler.UnitName, "packages") the ID of the
	// lease that failed. Such a host goes first in the next rollout of the
	// group. Absent when the host is in order.
	Broken map[string]string `json:"broken,omitempty"`
	Verbs  []protocol.Verb   `json:"verbs"`
	Needs  []protocol.Need   `json:"needs"`
	// Actions are the host actions its agent.yaml defines, as it reports
	// them.
	Actions []protocol.ActionInfo `json:"actions"`

	// Plan is the scheduler's view: slot state, attesting validators and
	// waiting reason.
	Plan scheduler.HostPlan `json:"plan"`
}

// HostListing is one row of the host list: what the overview, the host
// table and the selection need of a host. The rest of a HostSummary (the
// container internals, the node health, the needs in
// full, the broken marks) is in the host's details.
type HostListing struct {
	Name          string             `json:"name"`
	Roles         []string           `json:"roles"`
	Validators    uint64             `json:"validators"`
	LastSeen      time.Time          `json:"lastSeen"`
	Stale         bool               `json:"stale"`
	AgentVersion  string             `json:"agentVersion"`
	AgentProtocol int                `json:"agentProtocol"`
	Incompatible  string             `json:"incompatible,omitempty"`
	Containers    []ContainerListing `json:"containers"`
	OS            protocol.OSSummary `json:"os"`
	Pending       protocol.Pending   `json:"pending"`
	UptimeSeconds int64              `json:"uptimeSeconds"`
	// Usage is HostSummary.Resources in brief.
	Usage *HostUsage    `json:"usage,omitempty"`
	Hold  protocol.Hold `json:"hold"`
	// Holders are the holders of the holds (Hold.Holds), the host's own as
	// protocol.HolderHost.
	Holders []string           `json:"holders"`
	Offline *fleet.OfflineMark `json:"offline,omitempty"`
	Verbs   []protocol.Verb    `json:"verbs"`
	// Needs are the kinds of the host's open needs, one entry per need.
	Needs []NeedListing      `json:"needs"`
	Plan  scheduler.HostPlan `json:"plan"`
	// Actions are the host's actions (HostSummary.Actions).
	Actions []protocol.ActionInfo `json:"actions"`
}

// HostUsage is a host's resource use in whole percents, which is what the
// host list sorts and filters by: a number that moves less is a row that
// changes less often on the stream.
type HostUsage struct {
	// CPU is the busy share; absent until the agent has two readings.
	CPU *int `json:"cpu,omitempty"`
	// Memory is the share not available; absent when unknown.
	Memory *int `json:"memory,omitempty"`
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
	// Team is the team label of a managed container; served to
	// authenticated users only.
	Team string `json:"team,omitempty"`
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
	FirstSeen  time.Time `json:"firstSeen"`
	RemoteAddr string    `json:"remoteAddr"`
	// Assigned is the number of validators the validator ranges inventory
	// assigns to the host, active or not.
	Assigned uint64 `json:"assignedValidators"`

	// Inventory is nil until the agent has uploaded one.
	Inventory *protocol.Inventory `json:"inventory,omitempty"`
	// Leases are the host's recent leases, newest first.
	Leases []*fleet.Lease `json:"leases"`
	// History are the audit entries about this host, newest first.
	History []fleet.AuditEntry `json:"history"`
	// Reduced says that the login sees the host reduced (Access): what the
	// host's details leave out is hidden, not absent.
	Reduced bool `json:"reduced,omitempty"`
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

// RolloutView is a rollout as the views show it.
type RolloutView struct {
	fleet.Rollout

	// Policy is the policy the rollout answers to
	// (fleet.Settings.RolloutPolicy): manual waits for a person's approval.
	Policy fleet.Policy `json:"policy"`
	// PolicySource is what decided Policy of an image rollout
	// (fleet.Settings.GroupPolicy); empty for every other rollout.
	PolicySource fleet.PolicySource `json:"policySource,omitempty"`
}

// Rollouts is the body of GET /api/v1/rollouts.
type Rollouts struct {
	// Open are all non-terminal rollouts; Recent the latest terminal ones.
	Open   []*RolloutView `json:"open"`
	Recent []*RolloutView `json:"recent"`
	// Leases are the open leases; RecentLeases the latest closed ones.
	Leases       []*fleet.Lease `json:"leases"`
	RecentLeases []*fleet.Lease `json:"recentLeases"`
}

// SettingsBody is the body of GET and the answer of PUT /api/v1/settings.
type SettingsBody struct {
	Settings fleet.Settings `json:"settings"`
	Defaults fleet.Settings `json:"defaults"`
	// Kinds are the kinds of the managed containers of the fleet (FleetKinds).
	Kinds []protocol.ContainerKind `json:"kinds"`
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
