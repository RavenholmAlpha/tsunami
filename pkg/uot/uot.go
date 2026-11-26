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
	uotStreamReadBufBytes  = 64 * 1024
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

	// NAT mapping: remote AddrPort -> last seen UnixNano (shared across streamToUDP & udpToStream)
	natTable   map[netip.AddrPort]int64
	natTimeout time.Duration
	mu         sync.Mutex

	// Per-relay resolve caches owned exclusively by the streamToUDP goroutine.
	resolveCache   map[string]cachedUDPAddr
	ipResolveCache map[netip.AddrPort]cachedUDPAddr
	resolve        func(string) (*net.UDPAddr, error) // resolves and validates the destination
}

// NewRelay creates a new UoT relay for the given stream. resolve resolves a
// UoT destination exactly once and enforces the address policy; nil accepts
// any destination.
func NewRelay(stream io.ReadWriteCloser, resolve func(string) (*net.UDPAddr, error)) *Relay {
	return &Relay{
		stream:         stream,
		natTable:       make(map[netip.AddrPort]int64),
		resolveCache:   make(map[string]cachedUDPAddr),
		ipResolveCache: make(map[netip.AddrPort]cachedUDPAddr),
		natTimeout:     120 * time.Second,
		resolve:        resolve,
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

func (r *Relay) natRefreshStepNs() int64 {
	step := r.natTimeout / 4
	if step > time.Second {
		step = time.Second
	}
	if step <= 0 {
		return 0
	}
	return step.Nanoseconds()
}

func (r *Relay) touchNAT(ap netip.AddrPort, nowNs int64, lastAP *netip.AddrPort, lastTouchNs *int64, refreshStepNs int64) {
	if ap == *lastAP && refreshStepNs > 0 && nowNs-*lastTouchNs < refreshStepNs {
		return
	}
	r.mu.Lock()
	r.natTable[ap] = nowNs
	r.mu.Unlock()
	*lastAP = ap
	*lastTouchNs = nowNs
}

func (r *Relay) resolveDomainCached(target string, now time.Time) (netip.AddrPort, bool) {
	if r.resolve == nil {
		return netip.AddrPort{}, false
	}

	if cached, ok := r.resolveCache[target]; ok {
		if now.Before(cached.expireAt) {
			return cached.addrPort, true
		}
		delete(r.resolveCache, target)
	}

	udpAddr, err := r.resolve(target)
	if err != nil || udpAddr == nil {
		return netip.AddrPort{}, false
	}
	addr, ok := netip.AddrFromSlice(udpAddr.IP)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := netip.AddrPortFrom(addr.Unmap(), uint16(udpAddr.Port))

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
	return ap, true
}

func (r *Relay) resolveIPCached(rawAP netip.AddrPort, now time.Time) (netip.AddrPort, bool) {
	if r.resolve == nil {
		return netip.AddrPort{}, false
	}

	if cached, ok := r.ipResolveCache[rawAP]; ok {
		if now.Before(cached.expireAt) {
			return cached.addrPort, true
		}
		delete(r.ipResolveCache, rawAP)
	}

	// Format address string only once on cache miss to invoke the policy resolver.
	udpAddr, err := r.resolve(rawAP.String())
	if err != nil || udpAddr == nil {
		return netip.AddrPort{}, false
	}
	addr, ok := netip.AddrFromSlice(udpAddr.IP)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := netip.AddrPortFrom(addr.Unmap(), uint16(udpAddr.Port))

	if len(r.ipResolveCache) >= maxResolveCacheEntries {
		for k, v := range r.ipResolveCache {
			if !now.Before(v.expireAt) {
				delete(r.ipResolveCache, k)
			}
		}
		if len(r.ipResolveCache) >= maxResolveCacheEntries {
			for k := range r.ipResolveCache {
				delete(r.ipResolveCache, k)
				break
			}
		}
	}
	r.ipResolveCache[rawAP] = cachedUDPAddr{addrPort: ap, expireAt: now.Add(resolveCacheTTL)}
	return ap, true
}

// readUoTPacket decodes one UoT packet directly from br into payloadBuf.
// IPv4/IPv6 destinations are decoded straight into netip.AddrPort with zero
// string allocations.
func readUoTPacket(br *bufio.Reader, payloadBuf []byte, domainScratch *[258]byte) (ap netip.AddrPort, domainTarget string, isIP bool, data []byte, err error) {
	atyp, err := br.ReadByte()
	if err != nil {
		return netip.AddrPort{}, "", false, nil, err
	}

	var dataLen int
	switch atyp {
	case protocol.AtypIPv4:
		var hdr [8]byte // 4B IPv4 + 2B port + 2B length
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return netip.AddrPort{}, "", false, nil, err
		}
		ip := netip.AddrFrom4([4]byte(hdr[:4]))
		port := binary.BigEndian.Uint16(hdr[4:6])
		dataLen = int(binary.BigEndian.Uint16(hdr[6:8]))
		ap = netip.AddrPortFrom(ip, port)
		isIP = true

	case protocol.AtypIPv6:
		var hdr [20]byte // 16B IPv6 + 2B port + 2B length
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return netip.AddrPort{}, "", false, nil, err
		}
		ip := netip.AddrFrom16([16]byte(hdr[:16])).Unmap()
		port := binary.BigEndian.Uint16(hdr[16:18])
		dataLen = int(binary.BigEndian.Uint16(hdr[18:20]))
		ap = netip.AddrPortFrom(ip, port)
		isIP = true

	case protocol.AtypDomain:
		domainLenByte, err := br.ReadByte()
		if err != nil {
			return netip.AddrPort{}, "", false, nil, err
		}
		domainLen := int(domainLenByte)
		// Read domain + 2B port + 2B length in one call.
		if _, err := io.ReadFull(br, domainScratch[:domainLen+4]); err != nil {
			return netip.AddrPort{}, "", false, nil, err
		}
		port := binary.BigEndian.Uint16(domainScratch[domainLen : domainLen+2])
		dataLen = int(binary.BigEndian.Uint16(domainScratch[domainLen+2 : domainLen+4]))
		var out [272]byte
		b := append(out[:0], domainScratch[:domainLen]...)
		b = append(b, ':')
		b = strconv.AppendUint(b, uint64(port), 10)
		domainTarget = string(b)

	default:
		return netip.AddrPort{}, "", false, nil, fmt.Errorf("uot: unknown ATYP: 0x%02x", atyp)
	}

	data = payloadBuf[:dataLen]
	if _, err := io.ReadFull(br, data); err != nil {
		return netip.AddrPort{}, "", false, nil, err
	}
	return ap, domainTarget, isIP, data, nil
}

// hasCompleteBufferedUoTPacket checks via Peek whether br already holds a
// complete UoT packet in memory so batch sendmmsg never blocks waiting for a 2nd packet.
func hasCompleteBufferedUoTPacket(br *bufio.Reader) bool {
	avail := br.Buffered()
	if avail < 1+4+2+2 {
		return false
	}
	peeked, err := br.Peek(avail)
	if err != nil || len(peeked) < 1+4+2+2 {
		return false
	}
	var hdrLen int
	switch peeked[0] {
	case protocol.AtypIPv4:
		hdrLen = 1 + 4 + 2 + 2
	case protocol.AtypIPv6:
		hdrLen = 1 + 16 + 2 + 2
	case protocol.AtypDomain:
		hdrLen = 1 + 1 + int(peeked[1]) + 2 + 2
	default:
		return false
	}
	if len(peeked) < hdrLen {
		return false
	}
	dataLen := int(binary.BigEndian.Uint16(peeked[hdrLen-2 : hdrLen]))
	return len(peeked) >= hdrLen+dataLen
}

// streamToUDPSingle reads UoT packets from the stream and sends them as UDP
// using a 64 KiB buffered reader and zero-string IP resolution cache.
func (r *Relay) streamToUDPSingle() {
	defer r.cleanupNAT()

	br := bufio.NewReaderSize(r.stream, uotStreamReadBufBytes)
	payloadBuf := make([]byte, 65535)
	var domainScratch [258]byte
	var lastAP netip.AddrPort
	var lastTouchNs int64
	refreshStepNs := r.natRefreshStepNs()

	for {
		rawAP, domainTarget, isIP, data, err := readUoTPacket(br, payloadBuf, &domainScratch)
		if err != nil {
			return
		}
		if r.resolve == nil {
			continue
		}
		now := time.Now()
		var ap netip.AddrPort
		var ok bool
		if isIP {
			ap, ok = r.resolveIPCached(rawAP, now)
		} else {
			ap, ok = r.resolveDomainCached(domainTarget, now)
		}
		if !ok {
			continue
		}
		r.touchNAT(ap, now.UnixNano(), &lastAP, &lastTouchNs, refreshStepNs)
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
	sweepStepNs := (30 * time.Second).Nanoseconds()
	if half := (r.natTimeout / 2).Nanoseconds(); half > 0 && half < sweepStepNs {
		sweepStepNs = half
	}
	var lastDeadlineSet time.Time
	lastSweepNs := time.Now().UnixNano()
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

		// Check NAT table (with expiry and periodic sweep)
		nowNs := time.Now().UnixNano()
		r.mu.Lock()
		lastSeenNs, exists := r.natTable[ap]
		if exists && nowNs-lastSeenNs > timeoutNs {
			delete(r.natTable, ap)
			exists = false
		}
		if nowNs-lastSweepNs >= sweepStepNs {
			lastSweepNs = nowNs
			for k, v := range r.natTable {
				if nowNs-v > timeoutNs {
					delete(r.natTable, k)
				}
			}
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

// cleanupNAT removes all expired entries from the NAT table and resolve caches.
func (r *Relay) cleanupNAT() {
	now := time.Now()
	nowNs := now.UnixNano()
	timeoutNs := r.natTimeout.Nanoseconds()

	r.mu.Lock()
	for k, v := range r.natTable {
		if nowNs-v > timeoutNs {
			delete(r.natTable, k)
		}
	}
	r.mu.Unlock()

	for k, v := range r.resolveCache {
		if !now.Before(v.expireAt) {
			delete(r.resolveCache, k)
		}
	}
	for k, v := range r.ipResolveCache {
		if !now.Before(v.expireAt) {
			delete(r.ipResolveCache, k)
		}
	}
}
