// Package store persists the controller's state in SQLite
// (modernc.org/sqlite, WAL). The schema is the steps in migrations/.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store is the controller's persistence. Every method is safe for concurrent
// use and atomic on its own.
type Store interface {
	// Start opens the database and brings its schema up to date. It fails
	// with ErrSchemaVersion on a database it cannot upgrade.
	Start(ctx context.Context) error
	// Stop closes the database.
	Stop() error

	// RecordSync upserts the host from a verified certificate and its sync
	// request, sets last_seen and replaces the host's rows in needs. A host
	// that is new and reports a controller hold is created with the hold
	// wanted.
	RecordSync(ctx context.Context, sync *HostSync) (*SyncResult, error)
	// GetHost returns one host or ErrNotFound.
	GetHost(ctx context.Context, name string) (*fleet.Host, error)
	// ListHosts returns all hosts ordered by name.
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	// DeleteHost removes a host with its inventory, packages and per-slot
	// participation. Rollout, lease and audit history
	// is kept.
	DeleteHost(ctx context.Context, name string) error

	// SaveInventory replaces the host's inventory, packages and upgradable
	// rows and sets hosts.inventory_hash.
	SaveInventory(ctx context.Context, host string, inv *protocol.Inventory) error
	// GetInventory returns the stored inventory or ErrNotFound.
	GetInventory(ctx context.Context, host string) (*protocol.Inventory, error)
	// SearchPackages finds installed packages whose name contains query
	// (case-insensitive; an exact name match sorts first), grouped by name
	// and version.
	SearchPackages(ctx context.Context, query string, limit int) ([]PackageMatch, error)
	// ListPackages returns one page of the installed packages, one entry
	// per name: all of them, or those whose name contains the filter's
	// query. See PackageFilter for the order and the paging.
	ListPackages(ctx context.Context, filter PackageFilter) (*PackagePage, error)

	// SaveSlotParticipation upserts slot rows and replaces their per-host
	// rows.
	SaveSlotParticipation(ctx context.Context, rows []fleet.SlotParticipation) error
	// ListSlotParticipation returns the slot rows from fromSlot on, oldest
	// first, without their per-host part.
	ListSlotParticipation(ctx context.Context, fromSlot uint64) ([]fleet.SlotParticipation, error)
	// SumHostSlotParticipation sums the per-host rows from fromSlot on over
	// buckets of bucketSlots slots, ordered by bucket and host.
	SumHostSlotParticipation(
		ctx context.Context, fromSlot, bucketSlots uint64,
	) ([]fleet.HostSlotBucket, error)
	// SumHostSlotRuns returns the runs of slots from fromSlot to toSlot in
	// which a host missed attestations, ordered by host and first slot. Rows
	// of a host no more than gap slots apart are one run, and only runs of at
	// least minSlots slots are returned; for the hosts in perSlot every row
	// is a run of its own, and all of them are returned.
	SumHostSlotRuns(
		ctx context.Context, fromSlot, toSlot, gap, minSlots uint64, perSlot []string,
	) ([]fleet.HostSlotRun, error)
	// PruneSlotParticipation deletes the rows of slots older than beforeSlot.
	PruneSlotParticipation(ctx context.Context, beforeSlot uint64) error

	// ListRollouts returns rollouts with their hosts, newest first.
	ListRollouts(ctx context.Context, filter RolloutFilter) ([]*fleet.Rollout, error)
	// GetRollout returns one rollout or ErrNotFound.
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
	// AbortedKeys returns the keys of the aborted rollouts.
	AbortedKeys(ctx context.Context) ([]string, error)
	// ListLeases returns leases, newest first.
	ListLeases(ctx context.Context, filter LeaseFilter) ([]*fleet.Lease, error)
	// GetLease returns one lease or ErrNotFound.
	GetLease(ctx context.Context, id string) (*fleet.Lease, error)
	// ListDirectives returns directives, oldest first.
	ListDirectives(ctx context.Context, filter DirectiveFilter) ([]*fleet.Directive, error)
	// Commit applies a set of scheduler changes in one transaction.
	Commit(ctx context.Context, changes *Changes) error

	// LoadSettings returns defaults with every stored override applied.
	LoadSettings(ctx context.Context, defaults fleet.Settings) (fleet.Settings, error)
	// SaveSettings stores the fields of settings that differ from defaults
	// and removes overrides for fields that equal them.
	SaveSettings(ctx context.Context, defaults, settings fleet.Settings) error

	// AppendAudit appends entries; ID is assigned, a zero Time is set to now.
	AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error
	// ListAudit returns entries, newest first.
	ListAudit(ctx context.Context, filter AuditFilter) ([]fleet.AuditEntry, error)
}

// HostSync is the input of RecordSync.
type HostSync struct {
	Cert       *identity.Certificate
	RemoteAddr string
	Request    *protocol.SyncRequest
	At         time.Time
	// SyncInterval is the longest sync interval the answer to this sync
	// asks of the host (fleet.Host.SyncInterval).
	SyncInterval time.Duration
}

// SyncResult is the outcome of RecordSync.
type SyncResult struct {
	Host *fleet.Host
	// Created is true when the host was not known before.
	Created bool
	// Changed is false when the sync only confirmed the stored row: the same
	// certificate, address, validator range and status apart from its clock
	// fields. Only last_seen, the uptime and the sync interval were written.
	Changed bool
	// PreviousSeen is the host's last_seen before this sync; zero for a new
	// host.
	PreviousSeen time.Time
	// PreviousCertHash is set when the certificate hash changed.
	PreviousCertHash string
	// HoldAdopted is true when the host was created on hold because its
	// agent reported a controller hold (see HoldReasonAdopted).
	HoldAdopted bool
	// ValidatorsBefore is the validator range counted for the host before
	// this sync; Host.Validators is the one counted now.
	ValidatorsBefore *identity.ValidatorRange
	// ValidatorOverlaps names the other hosts for which validators of the
	// range this sync reported are counted, ordered by name. Those
	// validators stay with them, unless such a host has been silent for
	// ValidatorClaimTTL: then they are counted for this host from now on.
	ValidatorOverlaps []string
}

// ValidatorClaimTTL is how long the validators counted for a host are kept
// from other hosts that report them, measured from the host's last sync. A
// host that is gone for good must not keep the validators of its successor
// uncounted; a host that is only being worked on (its keys reshuffled) keeps
// syncing and keeps its claim.
const ValidatorClaimTTL = 10 * time.Minute

// HoldReasonAdopted is the hold reason of a host that reported a controller
// hold in the sync that created its row: a controller that does not know the
// host (a lost database, a forgotten host) keeps the hold instead of lifting
// it.
const HoldReasonAdopted = "held on the agent before this controller knew the host"

// PackageMatch is one package name with the versions installed across the
// fleet.
type PackageMatch struct {
	Name     string           `json:"name"`
	Versions []PackageVersion `json:"versions"`
}

// PackageFilter selects a page of package names.
//
// Without a query the names are in alphabetical order. With one they are
// ordered like a search: the exact match, then names that start with the
// query, then the rest, each alphabetically. A page continues after the last
// name of the page before (After is that page's Next), not at an offset: an
// inventory that changes between two pages neither repeats nor skips a name
// that was there for both.
type PackageFilter struct {
	// Query is a part of the name, ASCII case ignored; blank selects all.
	Query string
	// After is the Next of the page before; empty for the first page.
	After string
	// Limit bounds the names of the page (0 means
	// DefaultPackageSearchLimit, at most MaxPackageSearchLimit).
	Limit int
}

// PackagePage is one page of package names.
type PackagePage struct {
	Matches []PackageMatch
	// Total is the number of names the filter selects. It is counted for
	// the first page only (After empty) and -1 otherwise.
	Total int
	// Next continues after this page; empty when it is the last one.
	Next string
}

// ErrBadCursor is returned by ListPackages for an After that no page gave.
var ErrBadCursor = errors.New("store: not a page cursor")

// PackageVersion is one installed version and the hosts on it.
type PackageVersion struct {
	Version string   `json:"version"`
	Hosts   []string `json:"hosts"`
}

// RolloutFilter selects rollouts.
type RolloutFilter struct {
	// OpenOnly selects non-terminal rollouts.
	OpenOnly bool
	// Limit bounds the result; 0 means no limit.
	Limit int
}

// LeaseFilter selects leases.
type LeaseFilter struct {
	// OpenOnly selects non-terminal leases.
	OpenOnly bool
	// Host selects one host's leases when non-empty.
	Host  string
	Limit int
}

// DirectiveFilter selects directives.
type DirectiveFilter struct {
	Host string
	// PendingOnly selects directives in state pending.
	PendingOnly bool
	// Rollouts selects the directives of these rollouts when non-empty.
	Rollouts []string
}

// AuditFilter selects audit entries.
type AuditFilter struct {
	// Target selects entries about one host, rollout or setting.
	Target string
	// ActionPrefix selects entries whose action starts with it.
	ActionPrefix string
	// BeforeID pages backwards: only entries with a smaller ID.
	BeforeID int64
	// Limit bounds the result; 0 means 100.
	Limit int
}

// Changes is one atomic set of scheduler writes. Rollouts, leases and
// directives are upserted by ID; a rollout's Hosts replace its member rows.
// Holds set the controller-side hold switch of hosts; a host that does not
// exist fails the whole set with ErrNotFound; so do Offline marks. Broken
// replaces the broken
// marks of hosts, Unproven their fleet.Host.Unproven; a host that does not
// exist is passed over.
type Changes struct {
	Rollouts   []*fleet.Rollout
	Leases     []*fleet.Lease
	Directives []*fleet.Directive
	Holds      []HostHold
	Offline    []HostOffline
	Broken     []HostBroken
	Unproven   []HostUnproven
	Audit      []fleet.AuditEntry
}

// HostUnproven is fleet.Host.Unproven of one host as it is from now on; a
// nil At clears it.
type HostUnproven struct {
	Host string
	At   *time.Time
}

// HostBroken is the broken marks of one host (fleet.Host.Broken) as they are
// from now on; none clears them.
type HostBroken struct {
	Host  string
	Marks map[string]string
}

// HostOffline is one change of a host's offline mark; a nil Mark lifts it.
type HostOffline struct {
	Host string
	Mark *fleet.OfflineMark
}

// HostHold is one change of a host's controller-side hold switch.
type HostHold struct {
	Host   string
	Wanted bool
	Reason string
	// By is fleet.Host.HoldBy: fleet.TeamHold for a hold set through a team
	// feature, empty for every other writer. It is not kept for a hold that
	// is lifted.
	By string
	// Actor is fleet.Host.HoldActor.
	Actor string
}

// Empty reports whether there is nothing to commit.
func (c *Changes) Empty() bool {
	return len(c.Rollouts) == 0 && len(c.Leases) == 0 &&
		len(c.Directives) == 0 && len(c.Holds) == 0 && len(c.Offline) == 0 &&
		len(c.Broken) == 0 && len(c.Unproven) == 0 && len(c.Audit) == 0
}
