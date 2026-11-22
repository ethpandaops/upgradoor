-- upgradoor controller schema, step 2: a host's validators come from the
-- devnet's validator ranges inventory, not from what its agent reports;
-- containers are configured by their kind and team labels, the roll groups
-- and the default group are gone.

ALTER TABLE hosts DROP COLUMN validators_start;
ALTER TABLE hosts DROP COLUMN validators_end;

-- The last good version of the inventory: half-open index ranges by
-- inventory host name, disjoint.
CREATE TABLE validator_ranges (
    start_index INTEGER PRIMARY KEY,
    end_index   INTEGER NOT NULL,
    host        TEXT NOT NULL
);

CREATE INDEX validator_ranges_host ON validator_ranges(host);

-- Where that version came from, for the next conditional fetch. One row.
CREATE TABLE validator_ranges_source (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    url           TEXT NOT NULL,
    etag          TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    fetched_at    INTEGER NOT NULL
);

-- The open rollouts of roll groups name units that no longer exist: they are
-- superseded, and the hosts' needs open rollouts of their kinds and teams.
UPDATE rollouts SET
    state = 'superseded',
    ended_at = updated_at,
    data = json_set(data,
        '$.state', 'superseded',
        '$.reason', 'replaced by the rollouts of the containers'' kinds and teams',
        '$.endedAt', strftime('%Y-%m-%dT%H:%M:%fZ', updated_at / 1000.0, 'unixepoch'))
WHERE kind = 'group' AND state IN ('active', 'halted');

-- The broken marks of the old units name nothing any more.
UPDATE hosts SET broken = (
    SELECT json_group_object(key, value) FROM json_each(hosts.broken) WHERE key NOT LIKE 'group:%'
) WHERE broken LIKE '%"group:%';

-- The policy of the default group: unlabelled containers follow policyImage.
DELETE FROM settings WHERE key = 'policyDefaultGroup';
