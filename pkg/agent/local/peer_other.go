//go:build !linux

package local

import "net"

// peerUID is unknown without SO_PEERCRED; the agent runs on Linux.
func peerUID(net.Conn) (int, bool) { return 0, false }
