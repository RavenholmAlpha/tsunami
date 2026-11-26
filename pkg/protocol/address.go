package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"strconv"
)

// ReadSocksAddr reads one SOCKS5-style address from r and returns host:port.
// It uses io.ReadFull and stack scratch buffers so an address may span multiple
// PSH frames safely while minimizing heap allocations.
func ReadSocksAddr(r io.Reader) (string, error) {
	var scratch [1 + 255 + 2]byte
	if _, err := io.ReadFull(r, scratch[:1]); err != nil {
		return "", fmt.Errorf("tsunami: read address type: %w", err)
	}

	switch scratch[0] {
	case AtypIPv4:
		if _, err := io.ReadFull(r, scratch[:6]); err != nil {
			return "", fmt.Errorf("tsunami: read IPv4 address: %w", err)
		}
		port := binary.BigEndian.Uint16(scratch[4:6])
		if port == 0 {
			return "", fmt.Errorf("tsunami: invalid address port 0")
		}
		ip := netip.AddrFrom4([4]byte(scratch[:4]))
		var out [32]byte
		b := ip.AppendTo(out[:0])
		b = append(b, ':')
		b = strconv.AppendUint(b, uint64(port), 10)
		return string(b), nil

	case AtypDomain:
		if _, err := io.ReadFull(r, scratch[:1]); err != nil {
			return "", fmt.Errorf("tsunami: read domain length: %w", err)
		}
		domainLen := int(scratch[0])
		if domainLen == 0 {
			return "", fmt.Errorf("tsunami: empty domain")
		}
		if _, err := io.ReadFull(r, scratch[:domainLen+2]); err != nil {
			return "", fmt.Errorf("tsunami: read domain: %w", err)
		}
		port := binary.BigEndian.Uint16(scratch[domainLen : domainLen+2])
		if port == 0 {
			return "", fmt.Errorf("tsunami: invalid address port 0")
		}
		var out [272]byte
		b := append(out[:0], scratch[:domainLen]...)
		b = append(b, ':')
		b = strconv.AppendUint(b, uint64(port), 10)
		return string(b), nil

	case AtypIPv6:
		if _, err := io.ReadFull(r, scratch[:18]); err != nil {
			return "", fmt.Errorf("tsunami: read IPv6 address: %w", err)
		}
		port := binary.BigEndian.Uint16(scratch[16:18])
		if port == 0 {
			return "", fmt.Errorf("tsunami: invalid address port 0")
		}
		ip := netip.AddrFrom16([16]byte(scratch[:16]))
		var out [64]byte
		b := append(out[:0], '[')
		b = ip.AppendTo(b)
		b = append(b, ']', ':')
		b = strconv.AppendUint(b, uint64(port), 10)
		return string(b), nil

	default:
		return "", fmt.Errorf("tsunami: unknown address type: 0x%02x", scratch[0])
	}
}
