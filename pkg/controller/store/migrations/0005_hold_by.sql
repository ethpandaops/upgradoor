-- Who set the controller hold of a host: 'team' when it was set through a
-- team feature (a member of a team that owns a container of the host), ''
-- otherwise: staff, the scheduler, the end of a lease, or a build from before
-- this column. A hold set through a team feature is the only one a team
-- feature lifts.
ALTER TABLE hosts ADD COLUMN hold_by TEXT NOT NULL DEFAULT '';
