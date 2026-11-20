package shaping

import (
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

const (
	frameTypeDummy = 0x00
	frameTypeData  = 0x01

	outerHeaderLen = 3 // 1 byte type + 2 bytes bodyLen
	dataLenField   = 2 // 2 bytes inner payload length
	dataHeaderLen  = outerHeaderLen + dataLenField
	maxBodyLen     = 0xFFFF

	defaultWriteTimeout = 5 * time.Second
)

var errInvalidFrame = errors.New("shaping: invalid frame")

// Conn wraps a net.Conn with variable-size random-padded data framing,
// bounded backpressure buffering, and sparse random idle chaff.
type Conn struct {
	inner net.Conn
	cfg   Config

	// Write side
	writeMu     sync.Mutex
	writeCond   *sync.Cond
	writeBuf    []byte
	writeNotify chan struct{}
	writeDone   chan struct{}
	closedFlag  bool
	framePool   sync.Pool

	// Read side
	readMu    sync.Mutex
	readBuf   []byte
	readFrame []byte

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

// Wrap creates a shaped connection. The returned Conn implements net.Conn.
func Wrap(conn net.Conn, cfg Config) *Conn {
	cfg = normalizeConfig(cfg)
	payloadCap := cfg.FrameSize - dataHeaderLen

	c := &Conn{
		inner:       conn,
		cfg:         cfg,
		writeBuf:    make([]byte, 0, min(cfg.MaxBufferSize, cfg.FrameSize*cfg.BurstSlots)),
		writeNotify: make(chan struct{}, 1),
		writeDone:   make(chan struct{}),
		readFrame:   make([]byte, cfg.FrameSize),
		closed:      make(chan struct{}),
		framePool: sync.Pool{
			New: func() any {
				buf := make([]byte, cfg.FrameSize)
				return &buf
			},
		},
	}
	c.writeCond = sync.NewCond(&c.writeMu)

	_ = payloadCap
	go c.writeLoop()

	return c
}

func normalizeConfig(cfg Config) Config {
	if cfg.FrameSize < dataHeaderLen+3 {
		cfg.FrameSize = 1200
	}
	if cfg.FrameSize > maxBodyLen+outerHeaderLen {
		cfg.FrameSize = maxBodyLen + outerHeaderLen
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Millisecond
	}
	if cfg.BurstSlots < 1 {
		cfg.BurstSlots = 1
	}

	payloadCap := cfg.FrameSize - dataHeaderLen
	if cfg.MaxBufferSize <= 0 {
		cfg.MaxBufferSize = 4 << 20
	}
	if cfg.MaxBufferSize < payloadCap {
		cfg.MaxBufferSize = payloadCap
	}

	if cfg.IdleChaffMaxInterval > 0 {
		if cfg.IdleChaffMinInterval <= 0 {
			cfg.IdleChaffMinInterval = cfg.IdleChaffMaxInterval
		}
		if cfg.IdleChaffMaxInterval < cfg.IdleChaffMinInterval {
			cfg.IdleChaffMaxInterval = cfg.IdleChaffMinInterval
		}
		if cfg.IdleChaffMaxSize <= 0 {
			cfg.IdleChaffMaxSize = 1200
		}
		if cfg.IdleChaffMaxSize > maxBodyLen {
			cfg.IdleChaffMaxSize = maxBodyLen
		}
		if cfg.IdleChaffMinSize <= 0 {
			cfg.IdleChaffMinSize = 64
		}
		if cfg.IdleChaffMinSize > cfg.IdleChaffMaxSize {
			cfg.IdleChaffMinSize = cfg.IdleChaffMaxSize
		}
		if cfg.IdleChaffMaxBurst < 1 {
			cfg.IdleChaffMaxBurst = 3
		}
		if cfg.IdleChaffBurstMaxDelay < 0 {
			cfg.IdleChaffBurstMaxDelay = 0
		} else if cfg.IdleChaffBurstMaxDelay == 0 {
			cfg.IdleChaffBurstMaxDelay = 120 * time.Millisecond
		}
		if cfg.IdleChaffBurstMinDelay < 0 {
			cfg.IdleChaffBurstMinDelay = 0
		} else if cfg.IdleChaffBurstMinDelay == 0 && cfg.IdleChaffBurstMaxDelay >= 10*time.Millisecond {
			cfg.IdleChaffBurstMinDelay = 10 * time.Millisecond
		}
		if cfg.IdleChaffBurstMinDelay > cfg.IdleChaffBurstMaxDelay {
			cfg.IdleChaffBurstMinDelay = cfg.IdleChaffBurstMaxDelay
		}
	}

	return cfg
}

// Write buffers data for shaped transmission, applying backpressure if the
// bounded write buffer is full.
func (c *Conn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	if c.closedFlag {
		c.writeMu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if len(b) == 0 {
		c.writeMu.Unlock()
		return 0, nil
	}

	off := 0
	for off < len(b) {
		for len(c.writeBuf) >= c.cfg.MaxBufferSize && !c.closedFlag {
			c.notifyWriter()
			c.writeCond.Wait()
		}
		if c.closedFlag {
			c.writeMu.Unlock()
			return off, io.ErrClosedPipe
		}

		avail := c.cfg.MaxBufferSize - len(c.writeBuf)
		chunk := len(b) - off
		if chunk > avail {
			chunk = avail
		}
		c.writeBuf = append(c.writeBuf, b[off:off+chunk]...)
		off += chunk
		c.notifyWriter()
	}
	c.writeMu.Unlock()

	return off, nil
}

func (c *Conn) notifyWriter() {
	select {
	case c.writeNotify <- struct{}{}:
	default:
	}
}

// Read returns de-shaped data (dummy chaff frames stripped).
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

		var hdr [outerHeaderLen]byte
		if _, err := io.ReadFull(c.inner, hdr[:]); err != nil {
			return 0, err
		}

		frameType := hdr[0]
		bodyLen := int(binary.BigEndian.Uint16(hdr[1:3]))

		if cap(c.readFrame) < bodyLen {
			c.readFrame = make([]byte, bodyLen)
		}
		payload := c.readFrame[:bodyLen]
		if bodyLen > 0 {
			if _, err := io.ReadFull(c.inner, payload); err != nil {
				return 0, err
			}
		}

		switch frameType {
		case frameTypeDummy:
			continue
		case frameTypeData:
			if bodyLen < dataLenField {
				return 0, errInvalidFrame
			}
			dataLen := int(binary.BigEndian.Uint16(payload[:dataLenField]))
			if dataLen > bodyLen-dataLenField {
				return 0, errInvalidFrame
			}
			if dataLen == 0 {
				continue
			}

			data := payload[dataLenField : dataLenField+dataLen]
			c.readMu.Lock()
			c.readBuf = append(c.readBuf, data...)
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
		default:
			return 0, errInvalidFrame
		}
	}
}

func (c *Conn) writeLoop() {
	defer close(c.writeDone)

	payloadCap := c.cfg.FrameSize - dataHeaderLen

	var chaffTimer *time.Timer
	var chaffC <-chan time.Time
	if c.cfg.IdleChaffMaxInterval > 0 {
		chaffTimer = time.NewTimer(randDuration(c.cfg.IdleChaffMinInterval, c.cfg.IdleChaffMaxInterval))
		defer chaffTimer.Stop()
		chaffC = chaffTimer.C
	}

	resetChaff := func() {
		if chaffTimer != nil {
			chaffTimer.Reset(randDuration(c.cfg.IdleChaffMinInterval, c.cfg.IdleChaffMaxInterval))
		}
	}

	for {
		if c.hasBufferedData() {
			hasMore, ok := c.sendDataBurst(payloadCap)
			if !ok {
				return
			}
			resetChaff()
			if hasMore {
				paceTimer := time.NewTimer(c.cfg.Interval)
				select {
				case <-c.closed:
					paceTimer.Stop()
					return
				case <-paceTimer.C:
				}
			}
			continue
		}

		select {
		case <-c.closed:
			return
		case <-c.writeNotify:
			continue
		case <-chaffC:
			if !c.hasBufferedData() {
				if !c.sendIdleChaff() {
					return
				}
			}
			resetChaff()
		}
	}
}

func (c *Conn) hasBufferedData() bool {
	c.writeMu.Lock()
	n := len(c.writeBuf)
	c.writeMu.Unlock()
	return n > 0
}

func (c *Conn) sendDataBurst(payloadCap int) (hasMore bool, ok bool) {
	c.writeMu.Lock()
	buffered := len(c.writeBuf)
	c.writeMu.Unlock()

	if buffered == 0 {
		return false, true
	}

	slots := (buffered + payloadCap - 1) / payloadCap
	if slots > c.cfg.BurstSlots {
		slots = c.cfg.BurstSlots
	}

	timeout := c.cfg.Interval * time.Duration(slots+1)
	if timeout < defaultWriteTimeout {
		timeout = defaultWriteTimeout
	}
	_ = c.inner.SetWriteDeadline(time.Now().Add(timeout))

	for i := 0; i < slots; i++ {
		select {
		case <-c.closed:
			_ = c.inner.SetWriteDeadline(time.Time{})
			return false, false
		default:
		}

		c.writeMu.Lock()
		n := len(c.writeBuf)
		if n == 0 {
			c.writeMu.Unlock()
			break
		}
		if n > payloadCap {
			n = payloadCap
		}

		minTotal := dataHeaderLen + n
		totalLen := minTotal
		if minTotal < c.cfg.FrameSize {
			totalLen = minTotal + rand.IntN(c.cfg.FrameSize-minTotal+1)
		}
		bodyLen := totalLen - outerHeaderLen

		framep := c.framePool.Get().(*[]byte)
		frame := (*framep)[:totalLen]
		frame[0] = frameTypeData
		binary.BigEndian.PutUint16(frame[1:3], uint16(bodyLen))
		binary.BigEndian.PutUint16(frame[3:5], uint16(n))
		copy(frame[5:5+n], c.writeBuf[:n])
		if totalLen > minTotal {
			fillPseudoRandom(frame[minTotal:totalLen])
		}

		copy(c.writeBuf, c.writeBuf[n:])
		c.writeBuf = c.writeBuf[:len(c.writeBuf)-n]
		c.writeCond.Broadcast()
		c.writeMu.Unlock()

		_, err := c.inner.Write(frame)
		c.framePool.Put(framep)
		if err != nil {
			_ = c.inner.SetWriteDeadline(time.Time{})
			c.failClose()
			return false, false
		}
	}

	_ = c.inner.SetWriteDeadline(time.Time{})
	return c.hasBufferedData(), true
}

func (c *Conn) sendIdleChaff() bool {
	burst := 1
	if c.cfg.IdleChaffMaxBurst > 1 {
		burst += rand.IntN(c.cfg.IdleChaffMaxBurst)
	}

	for i := 0; i < burst; i++ {
		select {
		case <-c.closed:
			return false
		default:
		}
		if c.hasBufferedData() {
			return true
		}

		bodyLen := c.cfg.IdleChaffMinSize
		if c.cfg.IdleChaffMaxSize > c.cfg.IdleChaffMinSize {
			bodyLen += rand.IntN(c.cfg.IdleChaffMaxSize - c.cfg.IdleChaffMinSize + 1)
		}
		totalLen := outerHeaderLen + bodyLen
		frame := make([]byte, totalLen)
		frame[0] = frameTypeDummy
		binary.BigEndian.PutUint16(frame[1:3], uint16(bodyLen))
		if bodyLen > 0 {
			fillPseudoRandom(frame[outerHeaderLen:])
		}

		_ = c.inner.SetWriteDeadline(time.Now().Add(defaultWriteTimeout))
		_, err := c.inner.Write(frame)
		_ = c.inner.SetWriteDeadline(time.Time{})
		if err != nil {
			c.failClose()
			return false
		}

		if i+1 < burst && c.cfg.IdleChaffBurstMaxDelay > 0 {
			delay := randDuration(c.cfg.IdleChaffBurstMinDelay, c.cfg.IdleChaffBurstMaxDelay)
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-c.closed:
					timer.Stop()
					return false
				case <-c.writeNotify:
					timer.Stop()
					return true
				case <-timer.C:
				}
			}
		}
	}

	return true
}

func (c *Conn) failClose() {
	c.closeOnce.Do(func() {
		c.writeMu.Lock()
		c.closedFlag = true
		c.writeCond.Broadcast()
		c.writeMu.Unlock()
		close(c.closed)
		c.closeErr = c.inner.Close()
	})
}

func (c *Conn) Close() error {
	c.failClose()
	<-c.writeDone
	return c.closeErr
}

func (c *Conn) LocalAddr() net.Addr  { return c.inner.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.inner.RemoteAddr() }

func (c *Conn) SetDeadline(t time.Time) error      { return c.inner.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.inner.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.inner.SetWriteDeadline(t) }

func fillPseudoRandom(b []byte) {
	for len(b) >= 8 {
		binary.LittleEndian.PutUint64(b[:8], rand.Uint64())
		b = b[8:]
	}
	if len(b) > 0 {
		var tail [8]byte
		binary.LittleEndian.PutUint64(tail[:], rand.Uint64())
		copy(b, tail[:])
	}
}

func randDuration(minDur, maxDur time.Duration) time.Duration {
	if maxDur <= minDur {
		return minDur
	}
	return minDur + time.Duration(rand.Int64N(int64(maxDur-minDur)+1))
}
