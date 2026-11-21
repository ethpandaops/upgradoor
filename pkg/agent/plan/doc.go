// Package plan holds the agent's rules as pure functions over plain data:
// which containers are managed, what the host needs, what a lease may do,
// whether an override may be written, and the answers to directives.
//
// It does no I/O and never reads the clock; it imports only pkg/protocol,
// pkg/imageref and the standard library. What a container is recreated as
// comes from the host's files, never from the lease, and every step is
// checked against verbs, holds, managed set and policy before anything runs.
package plan
