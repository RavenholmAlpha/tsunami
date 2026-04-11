// Package proxy provides local SOCKS5 and HTTP proxy servers
// that forward connections through the TSUNAMI protocol.
package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/tsunami-protocol/tsunami/pkg/client"
)

// SOCKS5Server implements a local SOCKS5 proxy that forwards through TSUNAMI.
type SOCKS5Server struct {
	client   *client.Client
	listener net.Listener
	mu       sync.Mutex
	closed   bool
}

// NewSOCKS5Server creates a new SOCKS5 proxy server.
func NewSOCKS5Server(c *client.Client) *SOCKS5Server {
	return &SOCKS5Server{client: c}
}

// ListenAndServe starts the SOCKS5 server on the given address.
func (s *SOCKS5Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("socks5: listen %s: %w", addr, err)
	}
	s.listener = ln
	log.Printf("socks5: listening on %s", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			log.Printf("socks5: accept error: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

// Close shuts down the SOCKS5 server.
func (s *SOCKS5Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Addr returns the listener address (available after ListenAndServe starts).
func (s *SOCKS5Server) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// handleConn handles a single SOCKS5 client connection.
func (s *SOCKS5Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// --- SOCKS5 Handshake (RFC 1928) ---

	// 1. Read auth methods
	// +----+----------+----------+
	// |VER | NMETHODS | METHODS  |
	// +----+----------+----------+
	// | 1  |    1     | 1 to 255 |
	// +----+----------+----------+
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return
	}
	if header[0] != 0x05 {
		return // not SOCKS5
	}
	nmethods := int(header[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}

	// 2. Reply: no auth required
	// +----+--------+
	// |VER | METHOD |
	// +----+--------+
	// | 1  |   1    |
	// +----+--------+
	conn.Write([]byte{0x05, 0x00}) // No authentication

	// 3. Read request
	// +----+-----+-------+------+----------+----------+
	// |VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
	// +----+-----+-------+------+----------+----------+
	// | 1  |  1  | X'00' |  1   | Variable |    2     |
	// +----+-----+-------+------+----------+----------+
	var reqHeader [4]byte
	if _, err := io.ReadFull(conn, reqHeader[:]); err != nil {
		return
	}
	if reqHeader[0] != 0x05 {
		return
	}
	cmd := reqHeader[1]
	if cmd != 0x01 { // Only CONNECT supported
		// Reply with command not supported
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	atyp := reqHeader[3]
	var targetAddr string

	switch atyp {
	case 0x01: // IPv4
		var addr [4]byte
		if _, err := io.ReadFull(conn, addr[:]); err != nil {
			return
		}
		var port [2]byte
		if _, err := io.ReadFull(conn, port[:]); err != nil {
			return
		}
		p := binary.BigEndian.Uint16(port[:])
		targetAddr = fmt.Sprintf("%d.%d.%d.%d:%d", addr[0], addr[1], addr[2], addr[3], p)

	case 0x03: // Domain
		var domainLen [1]byte
		if _, err := io.ReadFull(conn, domainLen[:]); err != nil {
			return
		}
		domain := make([]byte, domainLen[0])
		if _, err := io.ReadFull(conn, domain); err != nil {
			return
		}
		var port [2]byte
		if _, err := io.ReadFull(conn, port[:]); err != nil {
			return
		}
		p := binary.BigEndian.Uint16(port[:])
		targetAddr = fmt.Sprintf("%s:%d", string(domain), p)

	case 0x04: // IPv6
		var addr [16]byte
		if _, err := io.ReadFull(conn, addr[:]); err != nil {
			return
		}
		var port [2]byte
		if _, err := io.ReadFull(conn, port[:]); err != nil {
			return
		}
		p := binary.BigEndian.Uint16(port[:])
		ip := net.IP(addr[:])
		targetAddr = fmt.Sprintf("[%s]:%d", ip.String(), p)

	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// 4. Open TSUNAMI stream to target
	stream, err := s.client.OpenStream(targetAddr)
	if err != nil {
		log.Printf("socks5: open stream to %s: %v", targetAddr, err)
		// Reply with general failure
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// 5. Reply success
	// +----+-----+-------+------+----------+----------+
	// |VER | REP |  RSV  | ATYP | BND.ADDR | BND.PORT |
	// +----+-----+-------+------+----------+----------+
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	// 6. Bidirectional relay: SOCKS client ←→ TSUNAMI stream
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(stream, conn)
		stream.Close()
	}()

	go func() {
		defer wg.Done()
		io.Copy(conn, stream)
	}()

	wg.Wait()
}
