-- upgradoor controller schema, step 6: settings changes.
-- The length of the initial deployment window is the controller's
-- deployWindow config key, no longer a setting.
-- The image policy of a team is per container kind (policyClients: kind ->
-- team -> policy). A stored policy of a team becomes one entry under every
-- kind of the team's managed containers ('other' for unlabelled ones); a team
-- that runs no container gets one under el, cl, vc, other and every kind of
-- the fleet.
-- The audit log is filtered by actor and time: the entries of people, of
-- agents and of SSH logins are few among the scheduler's and get partial
-- indexes.
-- SSH login groups the hosts report: one row per group, unique per host and
-- the host's sequence number; ssh_hosts marks the hosts that report them.

DELETE FROM settings WHERE key = 'deployWindow';

CREATE TEMP TABLE old_client_policies AS
SELECT j.key AS team, j.value AS policy
FROM settings s, json_each(s.value) j
WHERE s.key = 'policyClients' AND j.type = 'text';

CREATE TEMP TABLE team_kinds AS
SELECT DISTINCT
    json_extract(c.value, '$.team') AS team,
    CASE WHEN ifnull(json_extract(c.value, '$.kind'), '') = '' THEN 'other'
         ELSE json_extract(c.value, '$.kind') END AS kind
FROM hosts h, json_each(h.status, '$.containers') c
WHERE h.status IS NOT NULL AND json_valid(h.status)
  AND json_extract(c.value, '$.managed') = 1
  AND ifnull(json_extract(c.value, '$.team'), '') != '';

CREATE TEMP TABLE client_policy_pairs AS
SELECT k.kind, o.team, o.policy
FROM old_client_policies o JOIN team_kinds k ON k.team = o.team
UNION
SELECT a.kind, o.team, o.policy
FROM old_client_policies o,
     (SELECT kind FROM team_kinds UNION SELECT 'el' UNION SELECT 'cl' UNION SELECT 'vc' UNION SELECT 'other') a
WHERE NOT EXISTS (SELECT 1 FROM team_kinds k WHERE k.team = o.team);

DELETE FROM settings WHERE key = 'policyClients';

INSERT INTO settings (key, value)
SELECT 'policyClients', json_group_object(kind, json(teams))
FROM (SELECT kind, json_group_object(team, policy) AS teams FROM client_policy_pairs GROUP BY kind)
HAVING count(*) > 0;

DROP TABLE client_policy_pairs;
DROP TABLE team_kinds;
DROP TABLE old_client_policies;

CREATE INDEX audit_people ON audit(actor, id)
    WHERE actor != 'scheduler' AND substr(actor, 1, 6) != 'agent:' AND substr(actor, 1, 4) != 'ssh:';
CREATE INDEX audit_agents ON audit(actor, id) WHERE substr(actor, 1, 6) = 'agent:';
CREATE INDEX audit_ssh ON audit(actor, id) WHERE substr(actor, 1, 4) = 'ssh:';
CREATE INDEX audit_time ON audit(time);

CREATE TABLE ssh_logins (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    host        TEXT    NOT NULL,
    seq         INTEGER NOT NULL,
    identity    TEXT    NOT NULL DEFAULT '',
    source      TEXT    NOT NULL DEFAULT '',
    fingerprint TEXT    NOT NULL DEFAULT '',
    key_type    TEXT    NOT NULL DEFAULT '',
    login_user  TEXT    NOT NULL,
    from_ip     TEXT    NOT NULL,
    first_login INTEGER NOT NULL,
    last_logout INTEGER NOT NULL,
    sessions    INTEGER NOT NULL,
    open        INTEGER NOT NULL DEFAULT 0,
    closed      INTEGER NOT NULL DEFAULT 0,
    longest_ms  INTEGER NOT NULL DEFAULT 0,
    total_ms    INTEGER NOT NULL DEFAULT 0,
    end_unknown INTEGER NOT NULL DEFAULT 0,
    updated     INTEGER NOT NULL,
    UNIQUE (host, seq)
);

CREATE INDEX ssh_logins_time ON ssh_logins(first_login, id);
CREATE INDEX ssh_logins_host ON ssh_logins(host, first_login, id);
CREATE INDEX ssh_logins_identity ON ssh_logins(identity, first_login, id);
CREATE INDEX ssh_logins_user ON ssh_logins(login_user, first_login, id);
CREATE INDEX ssh_logins_unknown ON ssh_logins(first_login, id) WHERE identity = '';
CREATE INDEX ssh_logins_open ON ssh_logins(first_login, id) WHERE open > 0;
CREATE INDEX ssh_logins_unclosed ON ssh_logins(host) WHERE closed = 0;
CREATE INDEX ssh_logins_end ON ssh_logins(last_logout) WHERE closed = 1;

CREATE TABLE ssh_hosts (
    host       TEXT    PRIMARY KEY,
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    dropped    INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT ''
) WITHOUT ROWID;
