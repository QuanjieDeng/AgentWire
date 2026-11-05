//go:build linux

package daemon

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerAllowed accepts only connections from the same uid via SO_PEERCRED.
func peerAllowed(conn net.Conn) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var uid uint32
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		uid = cred.Uid
	}); err != nil || credErr != nil {
		return false
	}
	return uid == uint32(os.Getuid())
}
