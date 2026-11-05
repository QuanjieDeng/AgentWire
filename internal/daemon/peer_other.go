//go:build !linux && !windows

package daemon

import "net"

// The IPC directory is 0700, so only the same user can connect. Linux gets the
// stronger SO_PEERCRED check; other Unixes rely on directory permissions.
func peerAllowed(net.Conn) bool { return true }
