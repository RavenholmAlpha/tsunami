//go:build darwin

package transport

import (
	"net"

	"golang.org/x/sys/unix"
)

const defaultTCPNotSentLowat = 128 * 1024

func applyTCPBuffers(conn *net.TCPConn, cfg *TCPConfig) error {
	if rawConn, err := conn.SyscallConn(); err == nil {
		_ = rawConn.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, defaultTCPNotSentLowat)
		})
	}
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

func applyLinuxTCPOptions(_ *net.TCPConn, _ *TCPConfig) error {
	return nil
}
