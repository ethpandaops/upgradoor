//go:build !linux

package agent

import "net"

// peerUID is not known on systems without SO_PEERCRED; the agent runs on
// Linux.
func peerUID(net.Conn) (int, bool) { return 0, false }
