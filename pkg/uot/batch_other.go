//go:build !linux

package uot

import (
	"bufio"
	"net/netip"
	"time"
)

// streamToUDP reads UoT packets from the stream and sends them as UDP using a
// 64 KiB buffered reader and zero-string IP resolution cache.
func (r *Relay) streamToUDP() {
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

func (r *Relay) udpToStream() {
	r.udpToStreamSingle()
}
