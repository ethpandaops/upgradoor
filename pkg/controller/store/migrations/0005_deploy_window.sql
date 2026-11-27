-- upgradoor controller schema, step 5: the initial deployment window
-- (fleet.DeployWindowState). The controller opens it on its first start on a
-- fresh store. A store that already knows hosts or rollouts serves a running
-- fleet: it gets a window that is armed from the start.

CREATE TABLE deploy_window (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    started_at INTEGER NOT NULL,
    armed_at   INTEGER                   -- NULL while not armed
);

INSERT INTO deploy_window (id, started_at, armed_at)
SELECT 1, now, now
FROM (SELECT CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER) AS now)
WHERE EXISTS (SELECT 1 FROM hosts) OR EXISTS (SELECT 1 FROM rollouts);
