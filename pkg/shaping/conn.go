package shaping

import (
	"io"
	"net"
	"sync"
	"time"
)

// shapedFrame is a fixed-size wire frame used by the shaper.
// Format: [1 byte type] [payload...]
// Types: 0x00 = dummy (discard), 0x01 = data
const (
	frameTypeData  = 0x01
	frameTypeDummy = 0x00
)

// Conn wraps a net.Conn and shapes all traffic into constant-rate,
// fixed-size packets. Dummy packets fill idle periods.
type Conn struct {
	inner net.Conn
	cfg   Config

	// Write side
	writeBuf  []byte
	writeMu   sync.Mutex
	writeCond *sync.Cond
	writeDone chan struct{}

	// Read side
	readBuf []byte
	readMu  sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
}

// Wrap creates a shaped connection. The returned Conn implements net.Conn.
// The caller should use the returned Conn for all subsequent I/O.
func Wrap(conn net.Conn, cfg Config) *Conn {
	if cfg.FrameSize < 2 {
		cfg.FrameSize = 1200
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Millisecond
	}
	if cfg.BurstSlots < 1 {
		cfg.BurstSlots = 1
	}

	c := &Conn{
		inner:     conn,
		cfg:       cfg,
		writeBuf:  make([]byte, 0, cfg.FrameSize*cfg.BurstSlots),
		writeDone: make(chan struct{}),
		closed:    make(chan struct{}),
	}
	c.writeCond = sync.NewCond(&c.writeMu)

	go c.writeLoop()

	return c
}

// Write buffers data for shaped transmission.
func (c *Conn) Write(b []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	c.writeMu.Lock()
	c.writeBuf = append(c.writeBuf, b...)
	c.writeCond.Signal()
	c.writeMu.Unlock()

	return len(b), nil
}

// Read returns de-shaped data (dummy frames stripped).
func (c *Conn) Read(b []byte) (int, error) {
	for {
		// Try to return buffered data first
		c.readMu.Lock()
		if len(c.readBuf) > 0 {
			n := copy(b, c.readBuf)
			c.readBuf = c.readBuf[n:]
			c.readMu.Unlock()
			return n, nil
		}
		c.readMu.Unlock()

		// Read one shaped frame from the wire
		frame := make([]byte, c.cfg.FrameSize)
		_, err := io.ReadFull(c.inner, frame)
		if err != nil {
			return 0, err
		}

		frameType := frame[0]
		payload := frame[1:]

		if frameType == frameTypeDummy {
			continue // discard dummy frames
		}

		// Data frame: find actual data length.
		// Last 2 bytes encode the real payload length within this frame.
		if len(payload) < 2 {
			continue
		}
		dataLen := int(payload[len(payload)-2])<<8 | int(payload[len(payload)-1])
		if dataLen > len(payload)-2 {
			dataLen = len(payload) - 2
		}

		if dataLen == 0 {
			continue
		}

		c.readMu.Lock()
		c.readBuf = append(c.readBuf, payload[:dataLen]...)
		c.readMu.Unlock()

		// Return what we can
		c.readMu.Lock()
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		c.readMu.Unlock()
		return n, nil
	}
}

// writeLoop sends fixed-size frames at a constant interval.
func (c *Conn) writeLoop() {
	defer close(c.writeDone)

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	payloadCap := c.cfg.FrameSize - 3 // 1 byte type + 2 bytes length suffix

	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
			c.sendSlots(payloadCap)
		}
	}
}

// sendSlots sends up to BurstSlots frames in one tick.
func (c *Conn) sendSlots(payloadCap int) {
	c.writeMu.Lock()
	buffered := len(c.writeBuf)
	c.writeMu.Unlock()

	// Determine how many slots to send
	slots := 1
	if buffered > payloadCap {
		slots = (buffered + payloadCap - 1) / payloadCap
		if slots > c.cfg.BurstSlots {
			slots = c.cfg.BurstSlots
		}
	}

	for i := 0; i < slots; i++ {
		select {
		case <-c.closed:
			return
		default:
		}

		c.writeMu.Lock()
		n := len(c.writeBuf)
		if n > payloadCap {
			n = payloadCap
		}

		frame := make([]byte, c.cfg.FrameSize)

		if n > 0 {
			// Data frame
			frame[0] = frameTypeData
			copy(frame[1:], c.writeBuf[:n])
			// Encode actual data length in last 2 bytes
			frame[c.cfg.FrameSize-2] = byte(n >> 8)
			frame[c.cfg.FrameSize-1] = byte(n)
			c.writeBuf = c.writeBuf[n:]
		} else {
			// Dummy frame
			frame[0] = frameTypeDummy
		}
		c.writeMu.Unlock()

		// Use a write deadline to avoid blocking forever on slow/dead peers
		c.inner.SetWriteDeadline(time.Now().Add(c.cfg.Interval * 2))
		_, err := c.inner.Write(frame)
		c.inner.SetWriteDeadline(time.Time{})
		if err != nil {
			c.Close()
			return
		}
	}
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.writeCond.Signal()
	})
	<-c.writeDone
	return c.inner.Close()
}

func (c *Conn) LocalAddr() net.Addr  { return c.inner.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.inner.RemoteAddr() }

func (c *Conn) SetDeadline(t time.Time) error      { return c.inner.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error   { return c.inner.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error  { return c.inner.SetWriteDeadline(t) }
