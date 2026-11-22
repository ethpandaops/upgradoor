-- upgradoor controller schema, step 1: every table of an empty database.
-- A change to the schema is a further step (0002_<name>.sql, ...), never an
-- edit of a step that was released.
-- Times are INTEGER unix milliseconds (UTC). JSON columns hold the encoding
-- of the matching Go type in pkg/controller/fleet or pkg/protocol.

CREATE TABLE hosts (
    name             TEXT PRIMARY KEY,
    cert_hash        TEXT NOT NULL,
    validators_start INTEGER,            -- NULL for hosts without validators
    validators_end   INTEGER,
    roles            TEXT NOT NULL DEFAULT '[]',   -- JSON array
    first_seen       INTEGER NOT NULL,
    last_seen        INTEGER NOT NULL,
    remote_addr      TEXT NOT NULL DEFAULT '',
    status           TEXT,               -- JSON protocol.SyncRequest (last sync)
    inventory_hash   TEXT NOT NULL DEFAULT '',     -- hash of the stored inventory
    hold_wanted      INTEGER NOT NULL DEFAULT 0,
    hold_reason      TEXT NOT NULL DEFAULT '',
    -- Who set the controller hold: 'team' when it was set through a team
    -- feature (a member of a team that owns a container of the host), ''
    -- otherwise. A hold set through a team feature is the only one a team
    -- feature lifts.
    hold_by          TEXT NOT NULL DEFAULT '',
    -- The person who set the controller hold ('' when lifted).
    hold_actor       TEXT NOT NULL DEFAULT '',
    -- A person's mark that the host is known offline, as a JSON
    -- fleet.OfflineMark; '' while unmarked.
    offline          TEXT NOT NULL DEFAULT '',
    -- What the host's last lease failed on, as a JSON object: the group of
    -- rollouts (a roll group as 'group:<group>#<client>', or 'packages') to
    -- the ID of the lease that failed. The scheduler writes it; a host that
    -- is broken for the group of a rollout goes first in it.
    broken           TEXT NOT NULL DEFAULT '{}',
    -- When a lease of the host failed after it may have disrupted the host
    -- (fleet.Host.Unproven); NULL when none or once the host is proven back.
    -- The scheduler writes it; while it is set the host counts as down.
    unproven         INTEGER,
    -- Hash of the last status without what changes on every sync (sentAt,
    -- uptimeSeconds, the probes' heads and peer counts): a sync that reports
    -- the same hash changes nothing the scheduler decides on.
    status_hash      TEXT NOT NULL DEFAULT '',
    -- The longest sync interval the controller asked of the host in its
    -- last answer, in milliseconds; the host counts as stale only after
    -- missing a couple of those (fleet.Host.StaleAfter).
    sync_interval    INTEGER NOT NULL DEFAULT 0
);

-- OS and kernel part of the last inventory; packages and upgradable are
-- normalised below for search.
CREATE TABLE inventories (
    host         TEXT PRIMARY KEY REFERENCES hosts(name) ON DELETE CASCADE,
    hash         TEXT NOT NULL,
    collected_at INTEGER NOT NULL,
    os           TEXT NOT NULL,          -- JSON protocol.OSInfo
    kernels      TEXT NOT NULL           -- JSON protocol.KernelInfo
);

CREATE TABLE packages (
    host    TEXT NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    version TEXT NOT NULL,
    arch    TEXT NOT NULL,
    PRIMARY KEY (host, name, arch)
) WITHOUT ROWID;
CREATE INDEX packages_name ON packages(name, version);

CREATE TABLE upgradable (
    host      TEXT NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
    name      TEXT NOT NULL,
    arch      TEXT NOT NULL,
    current   TEXT NOT NULL,
    candidate TEXT NOT NULL,
    origin    TEXT NOT NULL DEFAULT '',
    security  INTEGER NOT NULL DEFAULT 0,
    blocked   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (host, name, arch)
) WITHOUT ROWID;
CREATE INDEX upgradable_name ON upgradable(name);

CREATE TABLE leases (
    id         TEXT PRIMARY KEY,
    host       TEXT NOT NULL,
    state      TEXT NOT NULL,
    granted_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    ended_at   INTEGER,
    data       TEXT NOT NULL             -- JSON fleet.Lease
);
CREATE INDEX leases_host ON leases(host, granted_at);
CREATE INDEX leases_state ON leases(state);

CREATE TABLE rollouts (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    key        TEXT NOT NULL,
    state      TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    ended_at   INTEGER,
    data       TEXT NOT NULL             -- JSON fleet.Rollout without Hosts
);
CREATE INDEX rollouts_key ON rollouts(key);
CREATE INDEX rollouts_state ON rollouts(state, created_at);

CREATE TABLE rollout_hosts (
    rollout_id TEXT NOT NULL REFERENCES rollouts(id) ON DELETE CASCADE,
    host       TEXT NOT NULL,
    state      TEXT NOT NULL,
    stage      INTEGER NOT NULL,
    need_id    TEXT NOT NULL DEFAULT '',
    lease_id   TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    skipped_by TEXT NOT NULL DEFAULT '',  -- the person behind a skip
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (rollout_id, host)
) WITHOUT ROWID;
CREATE INDEX rollout_hosts_host ON rollout_hosts(host);

CREATE TABLE directives (
    id         TEXT PRIMARY KEY,
    host       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    container  TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    -- Who asked and, for an override directive, what: a JSON object with the
    -- protocol.Directive fields "by", "override" and "withdraws"; '' when it
    -- has none of them.
    detail     TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL,
    rollout_id TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX directives_host ON directives(host, state);
-- The scheduler reads the pending directives of all hosts on every tick, and
-- directives are never pruned.
CREATE INDEX directives_state ON directives(state);
-- And the directives of the open manual rollouts, answered or not.
CREATE INDEX directives_rollout ON directives(rollout_id);

-- One row per overridden field of fleet.Settings, keyed by its JSON name.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL                  -- JSON value
) WITHOUT ROWID;

CREATE TABLE audit (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    time   INTEGER NOT NULL,
    actor  TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    detail TEXT                          -- JSON object
);
CREATE INDEX audit_target ON audit(target, id);
CREATE INDEX audit_action ON audit(action, id);

-- What the tracked validators did per slot, for the participation graph of
-- the web UI: how many had to attest in the slot, how many of those
-- attestations a canonical block carries, and whether the slot has a block.
-- The beacon service writes the rows and prunes them after its slot
-- retention. The scheduler's per-epoch liveness is summed from the same
-- counts in the beacon service's memory, not read from here.
CREATE TABLE slot_participation (
    slot     INTEGER PRIMARY KEY,
    proposed INTEGER NOT NULL,
    assigned INTEGER NOT NULL,
    included INTEGER NOT NULL
);

-- The hosts that deviated in a slot: the ones that missed attestations in it,
-- with their duties and what of them was included. A host that attested
-- everything has no row, so a healthy fleet stores nothing here however large
-- it is. No foreign key: the beacon service may store rows for a host that
-- is being forgotten.
CREATE TABLE slot_participation_hosts (
    slot     INTEGER NOT NULL,
    host     TEXT NOT NULL,
    assigned INTEGER NOT NULL,
    included INTEGER NOT NULL,
    PRIMARY KEY (slot, host)
) WITHOUT ROWID;
-- The runs of missed slots per host (SumHostSlotRuns) read the rows of a
-- host in slot order from here, without a sort.
CREATE INDEX slot_participation_hosts_host
    ON slot_participation_hosts(host, slot, assigned, included);
