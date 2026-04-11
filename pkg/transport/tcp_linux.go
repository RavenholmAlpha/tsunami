//go:build linux

package transport

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// applyLinuxTCPOptions applies Linux-specific TCP tuning: BBR, buffer sizes.
func applyLinuxTCPOptions(conn *net.TCPConn, cfg *TCPConfig) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("get syscall conn: %w", err)
	}

	var setErr error
	err = rawConn.Control(func(fd uintptr) {
		fdInt := int(fd)

		// Set TCP congestion control to BBR
		if cfg.ForceBBR {
			if e := unix.SetsockoptString(fdInt, unix.IPPROTO_TCP, unix.TCP_CONGESTION, "bbr"); e != nil {
				// BBR may not be available — non-fatal
				fmt.Printf("tsunami: failed to set BBR: %v (falling back to system default)\n", e)
			}
		}

		// Set send buffer size
		if cfg.SendBufferSize > 0 {
			if e := syscall.SetsockoptInt(fdInt, syscall.SOL_SOCKET, syscall.SO_SNDBUF, cfg.SendBufferSize); e != nil {
				setErr = fmt.Errorf("set SO_SNDBUF: %w", e)
				return
			}
		}

		// Set receive buffer size
		if cfg.RecvBufferSize > 0 {
			if e := syscall.SetsockoptInt(fdInt, syscall.SOL_SOCKET, syscall.SO_RCVBUF, cfg.RecvBufferSize); e != nil {
				setErr = fmt.Errorf("set SO_RCVBUF: %w", e)
				return
			}
		}
	})

	if err != nil {
		return err
	}
	return setErr
}
