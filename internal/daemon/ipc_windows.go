//go:build windows

package daemon

import (
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func currentSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

func ipcSocketPath() (string, error) {
	sid, err := currentSID()
	if err != nil {
		return "", err
	}
	return `\\.\pipe\agentwire-` + sid, nil
}

func listenIPC(path string) (net.Listener, error) {
	sid, err := currentSID()
	if err != nil {
		return nil, err
	}
	// Grant only the current user SID (plus LOCAL SYSTEM for service plumbing).
	cfg := &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;" + sid + ")"}
	l, err := winio.ListenPipe(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("监听命名管道失败：%w", err)
	}
	return l, nil
}

// The pipe ACL (current user SID only) already enforces the peer identity.
func peerAllowed(net.Conn) bool { return true }

func dialLocalIPC() (net.Conn, error) {
	path, err := ipcSocketPath()
	if err != nil {
		return nil, err
	}
	return winio.DialPipe(path, nil)
}

func dialIPCChannel(path string) (net.Conn, error) {
	timeout := 3 * time.Second
	return winio.DialPipe(path, &timeout)
}
