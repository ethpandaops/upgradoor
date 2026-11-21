// Package local is the host's own interfaces: the overrides file (Files),
// the CLI socket (Server, Client), the progress of rolls requested on the
// host (Rolls) and the host's audit log (Audit). It decides nothing; plan
// does.
//
// Text written to the overrides file or audit log is cut to one line, and a
// written entry or hold is parsed back before the file is replaced. The
// overrides file is read only as a regular file not reached through a
// symbolic link. The socket is for root and one group, and the caller's name
// comes from the kernel. Inputs are bounded; unknown fields are errors.
package local
