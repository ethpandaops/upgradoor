package agent

import (
	"net"
	"syscall"
)

// peerUID returns the Unix user at the other end of a connection of the
// local socket, as the kernel states it.
func peerUID(c net.Conn) (int, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, false
	}

	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}

	var (
		cred    *syscall.Ucred
		credErr error
	)

	err = raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil || credErr != nil || cred == nil {
		return 0, false
	}

	return int(cred.Uid), true
}
