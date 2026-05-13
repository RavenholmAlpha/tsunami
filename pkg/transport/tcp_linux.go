//go:build linux

package transport

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func applyTCPBuffers(conn *net.TCPConn, cfg *TCPConfig) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("get syscall conn: %w", err)
	}

	var warnings []string
	controlErr := rawConn.Control(func(fd uintptr) {
		if cfg.SendBufferSize > 0 {
			if warning := setLinuxSocketBuffer(int(fd), unix.SO_SNDBUF, unix.SO_SNDBUFFORCE, cfg.SendBufferSize, "/proc/sys/net/core/wmem_max"); warning != "" {
				warnings = append(warnings, "send: "+warning)
			}
		}
		if cfg.RecvBufferSize > 0 {
			if warning := setLinuxSocketBuffer(int(fd), unix.SO_RCVBUF, unix.SO_RCVBUFFORCE, cfg.RecvBufferSize, "/proc/sys/net/core/rmem_max"); warning != "" {
				warnings = append(warnings, "recv: "+warning)
			}
		}
	})
	if controlErr != nil {
		return controlErr
	}
	if len(warnings) > 0 {
		return fmt.Errorf("%s", strings.Join(warnings, "; "))
	}
	return nil
}

func setLinuxSocketBuffer(fd int, opt int, forceOpt int, size int, maxPath string) string {
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, forceOpt, size); err == nil {
		return ""
	} else {
		max, readErr := readLinuxSysctlInt(maxPath)
		if readErr == nil && size > max {
			return fmt.Sprintf("requested %d exceeds %s=%d and force failed (%v); leaving autotuning enabled", size, maxPath, max, err)
		}
		if err2 := unix.SetsockoptInt(fd, unix.SOL_SOCKET, opt, size); err2 != nil {
			return fmt.Sprintf("set buffer failed: force=%v fallback=%v", err, err2)
		}
	}
	return ""
}

func readLinuxSysctlInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

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
