//go:build linux

package uot

import (
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
	uotBatchSize      = 8
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

type batchReader interface {
	ReadBatch(ms []ipv6.Message, flags int) (int, error)
}

func (r *Relay) udpToStream() {
	var br batchReader
	if la, ok := r.udpConn.LocalAddr().(*net.UDPAddr); ok && la.IP != nil && la.IP.To4() != nil {
		br = ipv4.NewPacketConn(r.udpConn)
	} else {
		br = ipv6.NewPacketConn(r.udpConn)
	}

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
	var lastDeadlineSet time.Time
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
		out := slab.out[:0]

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

			uAddr := ap.Addr()
			if uAddr.Is4() {
				ip4 := uAddr.As4()
				var hdr [9]byte
				hdr[0] = protocol.AtypIPv4
				copy(hdr[1:5], ip4[:])
				binary.BigEndian.PutUint16(hdr[5:7], ap.Port())
				binary.BigEndian.PutUint16(hdr[7:9], uint16(n))
				out = append(out, hdr[:]...)
			} else {
				ip16 := uAddr.As16()
				var hdr [21]byte
				hdr[0] = protocol.AtypIPv6
				copy(hdr[1:17], ip16[:])
				binary.BigEndian.PutUint16(hdr[17:19], ap.Port())
				binary.BigEndian.PutUint16(hdr[19:21], uint16(n))
				out = append(out, hdr[:]...)
			}
			out = append(out, slab.bufs[i][:n]...)
		}
		r.mu.Unlock()

		slab.out = out
		if len(out) == 0 {
			continue
		}
		if _, err := r.stream.Write(out); err != nil {
			return
		}
	}
}
