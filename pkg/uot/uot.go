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
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// UDPPacket represents a single UDP packet in the UoT stream.
type UDPPacket struct {
	Addr net.Addr
	Data []byte
}

// Relay handles bidirectional UDP-over-TCP relay for a single stream.
type Relay struct {
	stream io.ReadWriteCloser

	// UDP socket for outbound
	udpConn *net.UDPConn

	// NAT mapping: remote addr → last seen time
	natTable   map[string]time.Time
	natTimeout time.Duration
	mu         sync.Mutex
}

// NewRelay creates a new UoT relay for the given stream.
func NewRelay(stream io.ReadWriteCloser) *Relay {
	return &Relay{
		stream:     stream,
		natTable:   make(map[string]time.Time),
		natTimeout: 120 * time.Second,
	}
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
	defer r.udpConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// Stream → UDP (client sends to target)
	go func() {
		defer wg.Done()
		r.streamToUDP()
	}()

	// UDP → Stream (target replies to client)
	go func() {
		defer wg.Done()
		r.udpToStream()
	}()

	wg.Wait()
	return nil
}

// streamToUDP reads UoT packets from the stream and sends them as UDP.
func (r *Relay) streamToUDP() {
	for {
		// Read ATYP
		var atypBuf [1]byte
		if _, err := io.ReadFull(r.stream, atypBuf[:]); err != nil {
			return
		}

		// Decode address
		target, addrLen, err := readSocksAddr(r.stream, atypBuf[0])
		if err != nil {
			return
		}
		_ = addrLen

		// Read data length
		var lenBuf [2]byte
		if _, err := io.ReadFull(r.stream, lenBuf[:]); err != nil {
			return
		}
		dataLen := binary.BigEndian.Uint16(lenBuf[:])

		// Read data
		data := make([]byte, dataLen)
		if _, err := io.ReadFull(r.stream, data); err != nil {
			return
		}

		// Resolve and send UDP
		udpAddr, err := net.ResolveUDPAddr("udp", target)
		if err != nil {
			continue
		}

		r.mu.Lock()
		r.natTable[udpAddr.String()] = time.Now()
		r.mu.Unlock()

		r.udpConn.WriteToUDP(data, udpAddr)
	}
}

// udpToStream reads UDP replies and writes them back as UoT packets.
func (r *Relay) udpToStream() {
	buf := make([]byte, 65535)
	for {
		r.udpConn.SetReadDeadline(time.Now().Add(r.natTimeout))
		n, remoteAddr, err := r.udpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		// Check NAT table
		r.mu.Lock()
		_, exists := r.natTable[remoteAddr.String()]
		r.mu.Unlock()
		if !exists {
			continue // drop unsolicited packets
		}

		// Encode UoT response packet
		packet, err := encodeUoTPacket(remoteAddr, buf[:n])
		if err != nil {
			continue
		}

		if _, err := r.stream.Write(packet); err != nil {
			return
		}
	}
}

// readSocksAddr reads a SOCKS5 address from the reader.
// Returns the decoded "host:port" string and the number of bytes consumed.
func readSocksAddr(r io.Reader, atyp byte) (string, int, error) {
	switch atyp {
	case protocol.AtypIPv4:
		var buf [4 + 2]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", 0, err
		}
		ip := net.IP(buf[:4])
		port := binary.BigEndian.Uint16(buf[4:6])
		return fmt.Sprintf("%s:%d", ip.String(), port), 7, nil

	case protocol.AtypDomain:
		var lenBuf [1]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return "", 0, err
		}
		domainLen := int(lenBuf[0])
		buf := make([]byte, domainLen+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", 0, err
		}
		domain := string(buf[:domainLen])
		port := binary.BigEndian.Uint16(buf[domainLen : domainLen+2])
		return fmt.Sprintf("%s:%d", domain, port), 2 + domainLen + 2, nil

	case protocol.AtypIPv6:
		var buf [16 + 2]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", 0, err
		}
		ip := net.IP(buf[:16])
		port := binary.BigEndian.Uint16(buf[16:18])
		return fmt.Sprintf("[%s]:%d", ip.String(), port), 19, nil

	default:
		return "", 0, fmt.Errorf("uot: unknown ATYP: 0x%02x", atyp)
	}
}

// encodeUoTPacket encodes a UDP response into UoT wire format.
func encodeUoTPacket(addr *net.UDPAddr, data []byte) ([]byte, error) {
	ip := addr.IP
	port := addr.Port

	var addrBytes []byte
	if ip4 := ip.To4(); ip4 != nil {
		addrBytes = make([]byte, 1+4+2)
		addrBytes[0] = protocol.AtypIPv4
		copy(addrBytes[1:5], ip4)
		binary.BigEndian.PutUint16(addrBytes[5:7], uint16(port))
	} else {
		addrBytes = make([]byte, 1+16+2)
		addrBytes[0] = protocol.AtypIPv6
		copy(addrBytes[1:17], ip.To16())
		binary.BigEndian.PutUint16(addrBytes[17:19], uint16(port))
	}

	// addr + length(2) + data
	packet := make([]byte, len(addrBytes)+2+len(data))
	copy(packet, addrBytes)
	binary.BigEndian.PutUint16(packet[len(addrBytes):len(addrBytes)+2], uint16(len(data)))
	copy(packet[len(addrBytes)+2:], data)

	return packet, nil
}
