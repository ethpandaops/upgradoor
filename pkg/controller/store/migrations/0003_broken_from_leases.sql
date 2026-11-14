-- Which hosts a change broke is derived from the lease table: a host whose
-- last lease for the same thing failed goes first. The mark that carried it
-- from rollout to rollout is gone. (The lists a rollout kept of what it
-- inherited and left are fields of its JSON; they are ignored when a row is
-- read and dropped when it is next written.)
ALTER TABLE rollout_hosts DROP COLUMN carried_from;
