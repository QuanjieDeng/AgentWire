//go:build !windows

package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

func ipcSocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	base := filepath.Join(dir, "agentwire")
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
