//go:build linux

package transport

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// applyLinuxTCPOptions applies Linux-specific TCP tuning: BBR congestion control.
// Buffer sizes are set cross-platform in ApplyTCPOptions via Go's net API.
func applyLinuxTCPOptions(conn *net.TCPConn, cfg *TCPConfig) error {
	if !cfg.ForceBBR {
		return nil
	}

	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("get syscall conn: %w", err)
	}

	return rawConn.Control(func(fd uintptr) {
		if e := unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, "bbr"); e != nil {
			// BBR may not be available — non-fatal
			fmt.Printf("tsunami: failed to set BBR: %v (falling back to system default)\n", e)
		}
	})
}
