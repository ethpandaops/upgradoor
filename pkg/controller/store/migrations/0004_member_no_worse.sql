-- upgradoor controller schema, step 4: a member done through a lease that
-- was released as no worse than before it is no success of its rollout.

ALTER TABLE rollout_hosts ADD COLUMN no_worse INTEGER NOT NULL DEFAULT 0;
