// Package store persists the controller's state in SQLite
// (modernc.org/sqlite, WAL). The schema is migrations/0001_init.sql.
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
	// Start opens the database and applies migrations.
	Start(ctx context.Context) error
	// Stop closes the database.
	Stop() error

	// RecordSync upserts the host from a verified certificate and its sync
	// request, sets last_seen and replaces the host's rows in needs. The
	// stored generation never goes down. A host that is new and reports a
	// controller hold is created with the hold wanted.
	RecordSync(ctx context.Context, sync *HostSync) (*SyncResult, error)
	// GetHost returns one host or ErrNotFound.
	GetHost(ctx context.Context, name string) (*fleet.Host, error)
	// ListHosts returns all hosts ordered by name.
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	// SetHostHold sets the controller-side hold switch.
	SetHostHold(ctx context.Context, name string, wanted bool, reason string) error
	// DeleteHost removes a host with its inventory, packages, needs and
	// liveness. Rollout, lease and audit history is kept.
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

	// SaveLiveness upserts per-host liveness rows.
	SaveLiveness(ctx context.Context, rows []fleet.Liveness) error
	// ListLiveness returns the rows of the newest `epochs` epochs for which
	// any row exists, newest epoch first.
	ListLiveness(ctx context.Context, epochs int) ([]fleet.Liveness, error)
	// PruneLiveness deletes rows older than beforeEpoch.
	PruneLiveness(ctx context.Context, beforeEpoch uint64) error

	// ListRollouts returns rollouts with their hosts, newest first.
	ListRollouts(ctx context.Context, filter RolloutFilter) ([]*fleet.Rollout, error)
	// GetRollout returns one rollout or ErrNotFound.
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
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
}

// SyncResult is the outcome of RecordSync.
type SyncResult struct {
	Host *fleet.Host
	// Created is true when the host was not known before.
	Created bool
	// PreviousCertHash is set when the certificate hash changed.
	PreviousCertHash string
	// HoldAdopted is true when the host was created on hold because its
	// agent reported a controller hold (see HoldReasonAdopted).
	HoldAdopted bool
}

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
type Changes struct {
	Rollouts   []*fleet.Rollout
	Leases     []*fleet.Lease
	Directives []*fleet.Directive
	Audit      []fleet.AuditEntry
}

// Empty reports whether there is nothing to commit.
func (c *Changes) Empty() bool {
	return len(c.Rollouts) == 0 && len(c.Leases) == 0 &&
		len(c.Directives) == 0 && len(c.Audit) == 0
}
