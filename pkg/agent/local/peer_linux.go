package local

import (
	"net"
	"syscall"
)

// peerUID is the user the kernel names at the other end of a Unix socket
// connection (SO_PEERCRED).
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
