-- upgradoor controller schema, version 1.
-- Times are INTEGER unix milliseconds (UTC). JSON columns hold the encoding
-- of the matching Go type in pkg/controller/fleet or pkg/protocol.

CREATE TABLE hosts (
    name             TEXT PRIMARY KEY,
    cert_hash        TEXT NOT NULL,
    generation       INTEGER NOT NULL DEFAULT 0,
    validators_start INTEGER,            -- NULL for hosts without validators
    validators_end   INTEGER,
    roles            TEXT NOT NULL DEFAULT '[]',   -- JSON array
    first_seen       INTEGER NOT NULL,
    last_seen        INTEGER NOT NULL,
    remote_addr      TEXT NOT NULL DEFAULT '',
    status           TEXT,               -- JSON protocol.SyncRequest (last sync)
    inventory_hash   TEXT NOT NULL DEFAULT '',     -- hash of the stored inventory
    hold_wanted      INTEGER NOT NULL DEFAULT 0,
    hold_reason      TEXT NOT NULL DEFAULT ''
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

-- Open needs as last reported; replaced wholesale on every sync.
CREATE TABLE needs (
    host       TEXT NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
    id         TEXT NOT NULL,
    kind       TEXT NOT NULL,
    detail     TEXT NOT NULL,            -- JSON protocol.Need
    first_seen INTEGER NOT NULL,
    PRIMARY KEY (host, id)
) WITHOUT ROWID;

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
    state      TEXT NOT NULL,
    rollout_id TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX directives_host ON directives(host, state);

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

CREATE TABLE validator_liveness (
    host  TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    live  INTEGER NOT NULL,
    total INTEGER NOT NULL,
    PRIMARY KEY (host, epoch)
) WITHOUT ROWID;
CREATE INDEX validator_liveness_epoch ON validator_liveness(epoch);
