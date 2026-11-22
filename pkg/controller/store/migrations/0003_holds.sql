-- upgradoor controller schema, step 3: the agents own the holds; the
-- controller keeps none of its own. A member remembers whether a lease of its
-- rollout was granted for it. readyEpochs is no setting any more.

ALTER TABLE hosts DROP COLUMN hold_wanted;
ALTER TABLE hosts DROP COLUMN hold_reason;
ALTER TABLE hosts DROP COLUMN hold_by;
ALTER TABLE hosts DROP COLUMN hold_actor;

-- The offline mark loses its 'by': only the plain hold feature sets it.
UPDATE hosts SET offline = json_remove(offline, '$.by') WHERE offline LIKE '%"by"%';

ALTER TABLE rollout_hosts ADD COLUMN served INTEGER NOT NULL DEFAULT 0;

UPDATE rollout_hosts SET served = 1
WHERE lease_id != '' OR state IN ('leased', 'failed', 'cancelled');

DELETE FROM settings WHERE key = 'readyEpochs';
