//go:build !windows

package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func ipcSocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	base := filepath.Join(dir, "teamconnect")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	os.Chmod(base, 0o700)
	return filepath.Join(base, "daemon.sock"), nil
}

func listenIPC(path string) (net.Listener, error) {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("监听本机 IPC 失败：%w", err)
	}
	os.Chmod(path, 0o600)
	return l, nil
}

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

func dialLocalIPC() (net.Conn, error) {
	path, err := ipcSocketPath()
	if err != nil {
		return nil, err
	}
	return net.DialTimeout("unix", path, 3*time.Second)
}

func dialIPCChannel(path string) (net.Conn, error) {
	return net.DialTimeout("unix", path, 3*time.Second)
}
