// Package uot implements UDP-over-TCP relay using the sing-box UoT v2 protocol.
//
// Wire format (within a Stream):
//
//	+------+----------+-------+--------+------+
//	| ATYP | address  | port  | length | data |
//	+------+----------+-------+--------+------+
//	| 1B   | variable | u16be | u16be  | var  |
//	+------+----------+-------+--------+------+
//
// Each UDP packet is independently framed.
package uot

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

const (
	resolveCacheTTL        = 30 * time.Second
	maxResolveCacheEntries = 256
	uotUDPSocketBufBytes   = 4 << 20
)

// UDPPacket represents a single UDP packet in the UoT stream.
type UDPPacket struct {
	Addr net.Addr
	Data []byte
}

type cachedUDPAddr struct {
	addrPort netip.AddrPort
	expireAt time.Time
}

// Relay handles bidirectional UDP-over-TCP relay for a single stream.
type Relay struct {
	stream io.ReadWriteCloser

	// UDP socket for outbound
	udpConn   *net.UDPConn
	closeOnce sync.Once

	// NAT mapping: remote AddrPort -> last seen UnixNano
	natTable     map[netip.AddrPort]int64
	resolveCache map[string]cachedUDPAddr
	natTimeout   time.Duration
	mu           sync.Mutex
	resolve      func(string) (*net.UDPAddr, error) // resolves and validates the destination
}

// NewRelay creates a new UoT relay for the given stream. resolve resolves a
// UoT destination exactly once and enforces the address policy; nil accepts
// any destination.
func NewRelay(stream io.ReadWriteCloser, resolve func(string) (*net.UDPAddr, error)) *Relay {
	return &Relay{
		stream:       stream,
		natTable:     make(map[netip.AddrPort]int64),
		resolveCache: make(map[string]cachedUDPAddr),
		natTimeout:   120 * time.Second,
		resolve:      resolve,
	}
}

func (r *Relay) closeUDP() {
	r.closeOnce.Do(func() {
		if r.udpConn != nil {
			_ = r.udpConn.Close()
		}
	})
}

// Run starts the bidirectional UDP relay. Blocks until the stream closes.
func (r *Relay) Run() error {
	// Create UDP socket for outbound
	udpAddr, _ := net.ResolveUDPAddr("udp", ":0")
	var err error
	r.udpConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("uot: listen UDP: %w", err)
	}
	// Best-effort enlarge kernel UDP socket buffers to absorb QUIC/HTTP3 bursts.
	_ = r.udpConn.SetReadBuffer(uotUDPSocketBufBytes)
	_ = r.udpConn.SetWriteBuffer(uotUDPSocketBufBytes)
	defer r.closeUDP()

	var wg sync.WaitGroup
	wg.Add(2)

	// Stream → UDP (client sends to target)
	go func() {
		defer wg.Done()
		defer r.closeUDP()
		r.streamToUDP()
	}()

	// UDP → Stream (target replies to client)
	go func() {
		defer wg.Done()
		r.udpToStream()
		_ = r.stream.Close()
	}()

	wg.Wait()
	return nil
}

func (r *Relay) resolveCached(target string, now time.Time) (netip.AddrPort, bool) {
	if r.resolve == nil {
		return netip.AddrPort{}, false
	}

	r.mu.Lock()
	if cached, ok := r.resolveCache[target]; ok {
		if now.Before(cached.expireAt) {
			r.natTable[cached.addrPort] = now.UnixNano()
			r.mu.Unlock()
			return cached.addrPort, true
		}
		delete(r.resolveCache, target)
	}
	r.mu.Unlock()

	udpAddr, err := r.resolve(target)
	if err != nil || udpAddr == nil {
		return netip.AddrPort{}, false
	}
	addr, ok := netip.AddrFromSlice(udpAddr.IP)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := netip.AddrPortFrom(addr.Unmap(), uint16(udpAddr.Port))

	r.mu.Lock()
	if len(r.resolveCache) >= maxResolveCacheEntries {
		for k, v := range r.resolveCache {
			if !now.Before(v.expireAt) {
				delete(r.resolveCache, k)
			}
		}
		if len(r.resolveCache) >= maxResolveCacheEntries {
			for k := range r.resolveCache {
				delete(r.resolveCache, k)
				break
			}
		}
	}
	r.resolveCache[target] = cachedUDPAddr{addrPort: ap, expireAt: now.Add(resolveCacheTTL)}
	r.natTable[ap] = now.UnixNano()
	r.mu.Unlock()
	return ap, true
}

// streamToUDP reads UoT packets from the stream and sends them as UDP.
func (r *Relay) streamToUDP() {
	defer r.cleanupNAT()

	// Use a compact 4 KiB reader instead of 64 KiB: protocol.Stream already
	// buffers frames in memory, while 4 KiB coalesces small tests/pipes cleanly.
	br := bufio.NewReaderSize(r.stream, 4*1024)
	payloadBuf := make([]byte, 65535)
	var domainScratch [258]byte

	for {
		// Read ATYP
		atyp, err := br.ReadByte()
		if err != nil {
			return
		}

		// Decode address
		target, err := readSocksAddrScratch(br, atyp, &domainScratch)
		if err != nil {
			return
		}

		// Read data length
		var lenBuf [2]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return
		}
		dataLen := int(binary.BigEndian.Uint16(lenBuf[:]))

		// Read data into reusable buffer
		data := payloadBuf[:dataLen]
		if _, err := io.ReadFull(br, data); err != nil {
			return
		}

		if r.resolve == nil {
			continue
		}
		now := time.Now()
		ap, ok := r.resolveCached(target, now)
		if !ok {
			continue
		}
		_, _ = r.udpConn.WriteToUDPAddrPort(data, ap)
	}
}

// udpToStreamSingle reads UDP replies one datagram at a time and writes them
// back as UoT packets.
func (r *Relay) udpToStreamSingle() {
	const maxHeader = 1 + 16 + 2 + 2 // 21 bytes for IPv6 header
	pktBuf := make([]byte, maxHeader+65535)
	readBuf := pktBuf[maxHeader:]
	deadlineStep := r.natTimeout / 4
	if deadlineStep > time.Second {
		deadlineStep = time.Second
	}
	var lastDeadlineSet time.Time
	timeoutNs := r.natTimeout.Nanoseconds()

	for {
		now := time.Now()
		if lastDeadlineSet.IsZero() || deadlineStep <= 0 || now.Sub(lastDeadlineSet) >= deadlineStep {
			_ = r.udpConn.SetReadDeadline(now.Add(r.natTimeout))
			lastDeadlineSet = now
		}

		n, remoteAP, err := r.udpConn.ReadFromUDPAddrPort(readBuf)
		if err != nil {
			return
		}
		ap := netip.AddrPortFrom(remoteAP.Addr().Unmap(), remoteAP.Port())

		// Check NAT table (with expiry)
		nowNs := time.Now().UnixNano()
		r.mu.Lock()
		lastSeenNs, exists := r.natTable[ap]
		if exists && nowNs-lastSeenNs > timeoutNs {
			delete(r.natTable, ap)
			exists = false
		}
		r.mu.Unlock()
		if !exists {
			continue // drop unsolicited or expired packets
		}

		// Encode UoT response header right in front of pktBuf[maxHeader:maxHeader+n]
		addr := ap.Addr()
		var start int
		if addr.Is4() {
			start = maxHeader - (1 + 4 + 2 + 2)
			ip4 := addr.As4()
			pktBuf[start] = protocol.AtypIPv4
			copy(pktBuf[start+1:start+5], ip4[:])
			binary.BigEndian.PutUint16(pktBuf[start+5:start+7], ap.Port())
			binary.BigEndian.PutUint16(pktBuf[start+7:start+9], uint16(n))
		} else {
			start = 0
			ip16 := addr.As16()
			pktBuf[0] = protocol.AtypIPv6
			copy(pktBuf[1:17], ip16[:])
			binary.BigEndian.PutUint16(pktBuf[17:19], ap.Port())
			binary.BigEndian.PutUint16(pktBuf[19:21], uint16(n))
		}

		if _, err := r.stream.Write(pktBuf[start : maxHeader+n]); err != nil {
			return
		}
	}
}

func readSocksAddrScratch(r io.Reader, atyp byte, domainScratch *[258]byte) (string, error) {
	switch atyp {
	case protocol.AtypIPv4:
		var buf [6]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", err
		}
		ip := netip.AddrFrom4([4]byte(buf[:4]))
		port := binary.BigEndian.Uint16(buf[4:6])
		var out [32]byte
		b := ip.AppendTo(out[:0])
		b = append(b, ':')
		b = strconv.AppendUint(b, uint64(port), 10)
		return string(b), nil

	case protocol.AtypDomain:
		var lenBuf [1]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return "", err
		}
		domainLen := int(lenBuf[0])
		if _, err := io.ReadFull(r, domainScratch[:domainLen+2]); err != nil {
			return "", err
		}
		port := binary.BigEndian.Uint16(domainScratch[domainLen : domainLen+2])
		var out [272]byte
		b := append(out[:0], domainScratch[:domainLen]...)
		b = append(b, ':')
		b = strconv.AppendUint(b, uint64(port), 10)
		return string(b), nil

	case protocol.AtypIPv6:
		var buf [18]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", err
		}
		ip := netip.AddrFrom16([16]byte(buf[:16]))
		port := binary.BigEndian.Uint16(buf[16:18])
		var out [64]byte
		b := append(out[:0], '[')
		b = ip.AppendTo(b)
		b = append(b, ']', ':')
		b = strconv.AppendUint(b, uint64(port), 10)
		return string(b), nil

	default:
		return "", fmt.Errorf("uot: unknown ATYP: 0x%02x", atyp)
	}
}

// cleanupNAT removes all expired entries from the NAT table and resolve cache.
func (r *Relay) cleanupNAT() {
	r.mu.Lock()
	now := time.Now()
	nowNs := now.UnixNano()
	timeoutNs := r.natTimeout.Nanoseconds()
	for k, v := range r.natTable {
		if nowNs-v > timeoutNs {
			delete(r.natTable, k)
		}
	}
	for k, v := range r.resolveCache {
		if !now.Before(v.expireAt) {
			delete(r.resolveCache, k)
		}
	}
	r.mu.Unlock()
}
