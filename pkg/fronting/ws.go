package fronting

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	websocketGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsPooledFrameBufCap = 72 * 1024
)

var wsWriteBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, wsPooledFrameBufCap)
		return &b
	},
}

// IsWebSocketUpgrade reports whether r is a HTTP/1.1 WebSocket upgrade.
func IsWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerContainsToken(r.Header.Get("Connection"), "upgrade")
}

// UpgradeServer upgrades an accepted HTTP request to a WebSocket net.Conn.
func UpgradeServer(w http.ResponseWriter, r *http.Request, serverHeader string) (net.Conn, error) {
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, fmt.Errorf("fronting: missing websocket key")
	}
	if version := r.Header.Get("Sec-WebSocket-Version"); version != "13" {
		return nil, fmt.Errorf("fronting: unsupported websocket version %q", version)
	}

	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("fronting: websocket hijack: %w", err)
	}

	accept := websocketAccept(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n"
	if serverHeader != "" {
		response += "Server: " + serverHeader + "\r\n"
	}
	response += "\r\n"

	if _, err := brw.WriteString(response); err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	return NewWebSocketConn(conn, brw.Reader, false), nil
}

// ClientWebSocketHandshake performs a WebSocket client handshake on conn using
// DefaultUserAgent.
func ClientWebSocketHandshake(conn net.Conn, endpoint *url.URL, host string, key [32]byte) (net.Conn, error) {
	return ClientWebSocketHandshakeWithUserAgent(conn, endpoint, host, key, DefaultUserAgent)
}

// ClientWebSocketHandshakeWithUserAgent performs a WebSocket client handshake on
// conn with the specified User-Agent header.
func ClientWebSocketHandshakeWithUserAgent(conn net.Conn, endpoint *url.URL, host string, key [32]byte, userAgent string) (net.Conn, error) {
	wsKey, err := newWebSocketKey()
	if err != nil {
		return nil, err
	}
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}

	req, err := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	if host != "" {
		req.Host = host
	}
	if req.Host == "" {
		req.Host = endpoint.Host
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", wsKey)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if err := SignRequest(req, key, time.Now()); err != nil {
		return nil, err
	}

	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("fronting: websocket request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("fronting: websocket response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("fronting: websocket status %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!headerContainsToken(resp.Header.Get("Connection"), "upgrade") {
		return nil, fmt.Errorf("fronting: invalid websocket upgrade response")
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), websocketAccept(wsKey); got != want {
		return nil, fmt.Errorf("fronting: invalid websocket accept")
	}

	return NewWebSocketConn(conn, br, true), nil
}

// WebSocketConn adapts WebSocket frames into a stream-oriented net.Conn.
type WebSocketConn struct {
	conn       net.Conn
	reader     *bufio.Reader
	maskWrites bool

	readBuf     []byte
	readScratch []byte
	writeMu     sync.Mutex
}

// NewWebSocketConn creates a WebSocket net.Conn wrapper.
func NewWebSocketConn(conn net.Conn, reader *bufio.Reader, maskWrites bool) *WebSocketConn {
	if reader == nil {
		reader = bufio.NewReader(conn)
	}
	return &WebSocketConn{conn: conn, reader: reader, maskWrites: maskWrites}
}

func (c *WebSocketConn) Read(p []byte) (int, error) {
	for len(c.readBuf) == 0 {
		payload, err := c.readFrame()
		if err != nil {
			return 0, err
		}
		c.readBuf = payload
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *WebSocketConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.writeFrame(0x2, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *WebSocketConn) Close() error {
	c.writeMu.Lock()
	_ = c.writeFrame(0x8, nil)
	c.writeMu.Unlock()
	return c.conn.Close()
}

func (c *WebSocketConn) LocalAddr() net.Addr               { return c.conn.LocalAddr() }
func (c *WebSocketConn) RemoteAddr() net.Addr              { return c.conn.RemoteAddr() }
func (c *WebSocketConn) SetDeadline(t time.Time) error     { return c.conn.SetDeadline(t) }
func (c *WebSocketConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }
func (c *WebSocketConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *WebSocketConn) readFrame() ([]byte, error) {
	for {
		b1, err := c.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		b2, err := c.reader.ReadByte()
		if err != nil {
			return nil, err
		}

		opcode := b1 & 0x0f
		masked := b2&0x80 != 0
		length := uint64(b2 & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		if length > 16*1024*1024 {
			return nil, fmt.Errorf("fronting: websocket frame too large")
		}

		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
				return nil, err
			}
		}
		var payload []byte
		if length <= wsPooledFrameBufCap {
			if cap(c.readScratch) < int(length) {
				c.readScratch = make([]byte, wsPooledFrameBufCap)
			}
			payload = c.readScratch[:length]
		} else {
			payload = make([]byte, length)
		}
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			return nil, err
		}
		if masked {
			xorMaskBytes(payload, mask)
		}

		switch opcode {
		case 0x0, 0x1, 0x2:
			return payload, nil
		case 0x8:
			return nil, io.EOF
		case 0x9:
			c.writeMu.Lock()
			_ = c.writeFrame(0xA, payload)
			c.writeMu.Unlock()
		case 0xA:
			continue
		default:
			return nil, fmt.Errorf("fronting: unsupported websocket opcode 0x%x", opcode)
		}
	}
}

func (c *WebSocketConn) writeFrame(opcode byte, payload []byte) error {
	var headerBuf [14]byte
	header := headerBuf[:2]
	header[0] = 0x80 | opcode
	payloadLen := len(payload)
	switch {
	case payloadLen < 126:
		header[1] = byte(payloadLen)
	case payloadLen <= 0xffff:
		header[1] = 126
		binary.BigEndian.PutUint16(headerBuf[2:4], uint16(payloadLen))
		header = headerBuf[:4]
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(headerBuf[2:10], uint64(payloadLen))
		header = headerBuf[:10]
	}

	var mask [4]byte
	if c.maskWrites {
		header[1] |= 0x80
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		hdrLen := len(header)
		copy(headerBuf[hdrLen:hdrLen+4], mask[:])
		header = headerBuf[:hdrLen+4]
	}

	totalLen := len(header) + payloadLen
	var bufp *[]byte
	var frame []byte
	if totalLen <= wsPooledFrameBufCap {
		bufp = wsWriteBufPool.Get().(*[]byte)
		frame = (*bufp)[:totalLen]
	} else {
		frame = make([]byte, totalLen)
	}
	copy(frame, header)
	body := frame[len(header):]
	copy(body, payload)
	if c.maskWrites {
		xorMaskBytes(body, mask)
	}
	_, err := c.conn.Write(frame)
	if bufp != nil {
		*bufp = (*bufp)[:wsPooledFrameBufCap]
		wsWriteBufPool.Put(bufp)
	}
	return err
}

func xorMaskBytes(b []byte, mask [4]byte) {
	if len(b) >= 8 {
		m32 := binary.LittleEndian.Uint32(mask[:])
		m64 := uint64(m32) | uint64(m32)<<32
		for len(b) >= 8 {
			v := binary.LittleEndian.Uint64(b[:8]) ^ m64
			binary.LittleEndian.PutUint64(b[:8], v)
			b = b[8:]
		}
	}
	for i := range b {
		b[i] ^= mask[i&3]
	}
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func newWebSocketKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

func headerContainsToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
