-- The scheduler reads the pending directives of all hosts on every tick, and
-- directives are never pruned. The index on (host, state) serves one host's
-- directives only.
CREATE INDEX directives_state ON directives(state);
