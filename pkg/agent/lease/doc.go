// Package lease runs a lease on the host and owns the agent's state file:
// the lease with its operation, the open manual needs and directive answers.
//
// A lease is on disk before it is acted on and resumes from there after a
// restart or reboot. Nothing disruptive happens before the disrupted mark is
// on disk; a cancel and the start of a step are decided under one lock; what
// a container is recreated as comes from the host's files (plan.Recheck),
// never from the lease. It touches the machine only through host.Host.
package lease
