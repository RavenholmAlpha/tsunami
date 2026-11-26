//go:build linux

package uot

import (
	"bufio"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	uotBatchSize      = 32
	uotMaxUDPPacket   = 65535
	uotMaxFrameHeader = 1 + 16 + 2 + 2
)

type uotBatchSlab struct {
	bufs [uotBatchSize][uotMaxUDPPacket]byte
	out  []byte
}

var uotBatchSlabPool = sync.Pool{
	New: func() any {
		return &uotBatchSlab{
			out: make([]byte, 0, uotBatchSize*(uotMaxFrameHeader+1500)),
		}
	},
}

type batchConn interface {
	ReadBatch(ms []ipv6.Message, flags int) (int, error)
	WriteBatch(ms []ipv6.Message, flags int) (int, error)
}

func newBatchConn(udpConn *net.UDPConn) batchConn {
	if la, ok := udpConn.LocalAddr().(*net.UDPAddr); ok && la.IP != nil && la.IP.To4() != nil {
		return ipv4.NewPacketConn(udpConn)
	}
	return ipv6.NewPacketConn(udpConn)
}

func (r *Relay) streamToUDP() {
	defer r.cleanupNAT()

	br := bufio.NewReaderSize(r.stream, uotStreamReadBufBytes)
	bw := newBatchConn(r.udpConn)
	slab := uotBatchSlabPool.Get().(*uotBatchSlab)
	defer uotBatchSlabPool.Put(slab)

	var msgs [uotBatchSize]ipv6.Message
	var addrs [uotBatchSize]net.UDPAddr
	var ip4Bufs [uotBatchSize][4]byte
	var ip16Bufs [uotBatchSize][16]byte
	for i := range msgs {
		msgs[i].Buffers = [][]byte{slab.bufs[i][:]}
		msgs[i].Addr = &addrs[i]
	}

	var domainScratch [258]byte
	var lastAP netip.AddrPort
	var lastTouchNs int64
	refreshStepNs := r.natRefreshStepNs()
	batchSupported := true

	for {
		rawAP, domainTarget, isIP, data, err := readUoTPacket(br, slab.bufs[0][:], &domainScratch)
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
		nowNs := now.UnixNano()
		r.touchNAT(ap, nowNs, &lastAP, &lastTouchNs, refreshStepNs)

		// Single-packet fast path: when no additional complete packet is buffered
		// in br, write immediately with 0us delay and zero UDPAddr conversion.
		if !batchSupported || !hasCompleteBufferedUoTPacket(br) {
			_, _ = r.udpConn.WriteToUDPAddrPort(data, ap)
			continue
		}

		// Burst path: drain up to uotBatchSize complete buffered packets and flush
		// via a single Linux sendmmsg (WriteBatch) syscall.
		setBatchMsg(&msgs[0], &addrs[0], &ip4Bufs[0], &ip16Bufs[0], data, ap)
		count := 1
		for count < uotBatchSize && hasCompleteBufferedUoTPacket(br) {
			rAP, dTarget, ipFlag, d, rErr := readUoTPacket(br, slab.bufs[count][:], &domainScratch)
			if rErr != nil {
				break
			}
			var targetAP netip.AddrPort
			var valid bool
			if ipFlag {
				targetAP, valid = r.resolveIPCached(rAP, now)
			} else {
				targetAP, valid = r.resolveDomainCached(dTarget, now)
			}
			if !valid {
				continue
			}
			r.touchNAT(targetAP, nowNs, &lastAP, &lastTouchNs, refreshStepNs)
			setBatchMsg(&msgs[count], &addrs[count], &ip4Bufs[count], &ip16Bufs[count], d, targetAP)
			count++
		}

		sent := 0
		for sent < count {
			n, wErr := bw.WriteBatch(msgs[sent:count], 0)
			if wErr != nil {
				if errors.Is(wErr, syscall.ENOSYS) || errors.Is(wErr, syscall.EOPNOTSUPP) {
					batchSupported = false
					for i := sent; i < count; i++ {
						if ua, ok := msgs[i].Addr.(*net.UDPAddr); ok {
							_, _ = r.udpConn.WriteToUDP(msgs[i].Buffers[0], ua)
						}
					}
				}
				break
			}
			if n <= 0 {
				break
			}
			sent += n
		}
	}
}

func setBatchMsg(msg *ipv6.Message, ua *net.UDPAddr, ip4 *[4]byte, ip16 *[16]byte, data []byte, ap netip.AddrPort) {
	msg.Buffers[0] = data
	addr := ap.Addr()
	if addr.Is4() {
		*ip4 = addr.As4()
		ua.IP = ip4[:]
	} else {
		*ip16 = addr.As16()
		ua.IP = ip16[:]
	}
	ua.Port = int(ap.Port())
	ua.Zone = ""
}

type validUDPReply struct {
	idx int
	n   int
	ap  netip.AddrPort
}

func (r *Relay) udpToStream() {
	br := newBatchConn(r.udpConn)

	slab := uotBatchSlabPool.Get().(*uotBatchSlab)
	defer func() {
		if cap(slab.out) <= uotBatchSize*(uotMaxFrameHeader+4096) {
			slab.out = slab.out[:0]
		} else {
			slab.out = make([]byte, 0, uotBatchSize*(uotMaxFrameHeader+1500))
		}
		uotBatchSlabPool.Put(slab)
	}()

	var msgs [uotBatchSize]ipv6.Message
	for i := range msgs {
		msgs[i].Buffers = [][]byte{slab.bufs[i][:]}
	}

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

		count, err := br.ReadBatch(msgs[:], 0)
		if err != nil {
			if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
				r.udpToStreamSingle()
				return
			}
			return
		}
		if count <= 0 {
			continue
		}

		nowNs := time.Now().UnixNano()
		var valid [uotBatchSize]validUDPReply
		validCount := 0

		// Hold r.mu ONLY for NAT validation (and periodic expiry sweep); keep
		// all UoT framing and payload copies lock-free outside the mutex.
		r.mu.Lock()
		for i := 0; i < count; i++ {
			n := msgs[i].N
			if n <= 0 {
				continue
			}
			ua, ok := msgs[i].Addr.(*net.UDPAddr)
			if !ok || ua == nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(ua.IP)
			if !ok {
				continue
			}
			ap := netip.AddrPortFrom(addr.Unmap(), uint16(ua.Port))
			lastSeenNs, exists := r.natTable[ap]
			if exists && nowNs-lastSeenNs > timeoutNs {
				delete(r.natTable, ap)
				exists = false
			}
			if !exists {
				continue
			}
			valid[validCount] = validUDPReply{idx: i, n: n, ap: ap}
			validCount++
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

		if validCount == 0 {
			continue
		}

		out := slab.out[:0]
		for j := 0; j < validCount; j++ {
			item := valid[j]
			uAddr := item.ap.Addr()
			if uAddr.Is4() {
				ip4 := uAddr.As4()
				var hdr [9]byte
				hdr[0] = protocol.AtypIPv4
				copy(hdr[1:5], ip4[:])
				binary.BigEndian.PutUint16(hdr[5:7], item.ap.Port())
				binary.BigEndian.PutUint16(hdr[7:9], uint16(item.n))
				out = append(out, hdr[:]...)
			} else {
				ip16 := uAddr.As16()
				var hdr [21]byte
				hdr[0] = protocol.AtypIPv6
				copy(hdr[1:17], ip16[:])
				binary.BigEndian.PutUint16(hdr[17:19], item.ap.Port())
				binary.BigEndian.PutUint16(hdr[19:21], uint16(item.n))
				out = append(out, hdr[:]...)
			}
			out = append(out, slab.bufs[item.idx][:item.n]...)
		}

		slab.out = out
		if _, err := r.stream.Write(out); err != nil {
			return
		}
	}
}
