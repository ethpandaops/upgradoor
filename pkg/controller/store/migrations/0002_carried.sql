-- A member that was carried over as failed from the rollout this one
-- supersedes keeps the ID of that rollout. It used to be a prefix of the
-- member's error note, which state changes cleared.
ALTER TABLE rollout_hosts ADD COLUMN carried_from TEXT NOT NULL DEFAULT '';
