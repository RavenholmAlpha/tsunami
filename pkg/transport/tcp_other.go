//go:build !linux

package transport

import "net"

// applyLinuxTCPOptions is a no-op on non-Linux platforms.
// BBR and advanced TCP tuning are Linux-specific features.
func applyLinuxTCPOptions(conn *net.TCPConn, cfg *TCPConfig) error {
	// No platform-specific tuning available on this OS
	return nil
}
