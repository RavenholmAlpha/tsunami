package shaping

import (
	"io"
	"net"
	"sync"
	"time"
)

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
	writeDone chan struct{}
	framePool sync.Pool

	// Read side
	readBuf   []byte
	readFrame []byte
	readMu    sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
}

// Wrap creates a shaped connection. The returned Conn implements net.Conn.
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
		readFrame: make([]byte, cfg.FrameSize),
		writeDone: make(chan struct{}),
		closed:    make(chan struct{}),
		framePool: sync.Pool{
			New: func() any {
				buf := make([]byte, cfg.FrameSize)
				return &buf
			},
		},
	}

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
	c.writeMu.Unlock()

	return len(b), nil
}

// Read returns de-shaped data (dummy frames stripped).
func (c *Conn) Read(b []byte) (int, error) {
	for {
		c.readMu.Lock()
		if len(c.readBuf) > 0 {
			n := copy(b, c.readBuf)
			remaining := len(c.readBuf) - n
			if remaining == 0 {
				c.readBuf = c.readBuf[:0]
			} else {
				copy(c.readBuf, c.readBuf[n:])
				c.readBuf = c.readBuf[:remaining]
			}
			c.readMu.Unlock()
			return n, nil
		}
		c.readMu.Unlock()

		// Read one shaped frame from the wire using pre-allocated buffer
		_, err := io.ReadFull(c.inner, c.readFrame)
		if err != nil {
			return 0, err
		}

		if c.readFrame[0] == frameTypeDummy {
			continue
		}

		payload := c.readFrame[1:]
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

		// Copy into readBuf and return
		c.readMu.Lock()
		c.readBuf = append(c.readBuf, payload[:dataLen]...)
		n := copy(b, c.readBuf)
		remaining := len(c.readBuf) - n
		if remaining == 0 {
			c.readBuf = c.readBuf[:0]
		} else {
			copy(c.readBuf, c.readBuf[n:])
			c.readBuf = c.readBuf[:remaining]
		}
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

	slots := 1
	if buffered > payloadCap {
		slots = (buffered + payloadCap - 1) / payloadCap
		if slots > c.cfg.BurstSlots {
			slots = c.cfg.BurstSlots
		}
	}

	// Set deadline once for the entire batch
	deadline := time.Now().Add(c.cfg.Interval * time.Duration(slots+1))
	c.inner.SetWriteDeadline(deadline)

	for i := 0; i < slots; i++ {
		select {
		case <-c.closed:
			c.inner.SetWriteDeadline(time.Time{})
			return
		default:
		}

		framep := c.framePool.Get().(*[]byte)
		frame := *framep

		c.writeMu.Lock()
		n := len(c.writeBuf)
		if n > payloadCap {
			n = payloadCap
		}

		// Zero the frame
		clear(frame)

		if n > 0 {
			frame[0] = frameTypeData
			copy(frame[1:], c.writeBuf[:n])
			frame[c.cfg.FrameSize-2] = byte(n >> 8)
			frame[c.cfg.FrameSize-1] = byte(n)
			// Compact writeBuf to reclaim memory
			copy(c.writeBuf, c.writeBuf[n:])
			c.writeBuf = c.writeBuf[:len(c.writeBuf)-n]
		} else {
			frame[0] = frameTypeDummy
		}
		c.writeMu.Unlock()

		_, err := c.inner.Write(frame)
		c.framePool.Put(framep)
		if err != nil {
			c.inner.SetWriteDeadline(time.Time{})
			c.Close()
			return
		}
	}

	c.inner.SetWriteDeadline(time.Time{})
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	<-c.writeDone
	return c.inner.Close()
}

func (c *Conn) LocalAddr() net.Addr  { return c.inner.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.inner.RemoteAddr() }

func (c *Conn) SetDeadline(t time.Time) error      { return c.inner.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error   { return c.inner.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error  { return c.inner.SetWriteDeadline(t) }
