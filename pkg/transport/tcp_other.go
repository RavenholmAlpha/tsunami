//go:build !linux

package transport

import "net"

func applyTCPBuffers(conn *net.TCPConn, cfg *TCPConfig) error {
	if cfg.SendBufferSize > 0 {
		if err := conn.SetWriteBuffer(cfg.SendBufferSize); err != nil {
			return err
		}
	}
	if cfg.RecvBufferSize > 0 {
		if err := conn.SetReadBuffer(cfg.RecvBufferSize); err != nil {
			return err
		}
	}
	return nil
}

// applyLinuxTCPOptions is a no-op on non-Linux platforms.
// BBR and advanced TCP tuning are Linux-specific features.
func applyLinuxTCPOptions(conn *net.TCPConn, cfg *TCPConfig) error {
	// No platform-specific tuning available on this OS
	return nil
}
