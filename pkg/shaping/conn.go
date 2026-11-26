package shaping

import (
	"bufio"
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
	maxCoalesceWrite    = 128 * 1024
)

var errInvalidFrame = errors.New("shaping: invalid frame")

type byteRing struct {
	buf  []byte
	head int
	len  int
}

func newByteRing(initialCap int) byteRing {
	if initialCap <= 0 {
		return byteRing{}
	}
	return byteRing{buf: make([]byte, initialCap)}
}

func (r *byteRing) write(p []byte, maxCap int) int {
	avail := maxCap - r.len
	if avail <= 0 || len(p) == 0 {
		return 0
	}
	n := len(p)
	if n > avail {
		n = avail
	}
	need := r.len + n
	if need > len(r.buf) {
		newCap := len(r.buf) * 2
		if newCap < 32*1024 {
			newCap = 32 * 1024
		}
		for newCap < need {
			newCap *= 2
		}
		if newCap > maxCap {
			newCap = maxCap
		}
		newBuf := make([]byte, newCap)
		if r.len > 0 {
			first := len(r.buf) - r.head
			if first > r.len {
				first = r.len
			}
			copy(newBuf, r.buf[r.head:r.head+first])
			if first < r.len {
				copy(newBuf[first:], r.buf[:r.len-first])
			}
		}
		r.buf = newBuf
		r.head = 0
	}

	tail := (r.head + r.len) % len(r.buf)
	first := len(r.buf) - tail
	if first > n {
		first = n
	}
	copy(r.buf[tail:], p[:first])
	if first < n {
		copy(r.buf, p[first:n])
	}
	r.len += n
	return n
}

func (r *byteRing) readCopy(dst []byte) int {
	n := len(dst)
	if n > r.len {
		n = r.len
	}
	if n == 0 {
		return 0
	}
	first := len(r.buf) - r.head
	if first > n {
		first = n
	}
	copy(dst, r.buf[r.head:r.head+first])
	if first < n {
		copy(dst[first:], r.buf[:n-first])
	}
	r.head = (r.head + n) % len(r.buf)
	r.len -= n
	if r.len == 0 {
		r.head = 0
	}
	return n
}

// Conn wraps a net.Conn with variable-size random-padded data framing,
// bounded backpressure buffering, and sparse random idle chaff.
type Conn struct {
	inner       net.Conn
	innerReader *bufio.Reader
	cfg         Config

	// Write side
	writeMu          sync.Mutex
	writeCond        *sync.Cond
	writeWaiters     int
	writeRing        byteRing
	writeNotify      chan struct{}
	writeDone        chan struct{}
	closedFlag       bool
	burstBuf         []byte
	deadlineArmed    bool
	deadlineRefresh  time.Time

	// Read side
	readMu    sync.Mutex
	readBuf   []byte
	readOff   int
	readFrame []byte
	readHdr   [outerHeaderLen]byte

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

// Wrap creates a shaped connection. The returned Conn implements net.Conn.
func Wrap(conn net.Conn, cfg Config) *Conn {
	cfg = normalizeConfig(cfg)
	initCap := min(cfg.MaxBufferSize, cfg.FrameSize*cfg.BurstSlots)

	c := &Conn{
		inner:       conn,
		innerReader: bufio.NewReaderSize(conn, 64*1024),
		cfg:         cfg,
		writeRing:   newByteRing(initCap),
		writeNotify: make(chan struct{}, 1),
		writeDone:   make(chan struct{}),
		readFrame:   make([]byte, cfg.FrameSize),
		closed:      make(chan struct{}),
	}
	c.writeCond = sync.NewCond(&c.writeMu)

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
		cfg.MaxBufferSize = 256 << 10
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
		if c.writeRing.len >= c.cfg.MaxBufferSize && !c.closedFlag {
			c.writeWaiters++
			for c.writeRing.len >= c.cfg.MaxBufferSize && !c.closedFlag {
				c.notifyWriter()
				c.writeCond.Wait()
			}
			c.writeWaiters--
		}
		if c.closedFlag {
			c.writeMu.Unlock()
			return off, io.ErrClosedPipe
		}

		wasEmpty := c.writeRing.len == 0
		written := c.writeRing.write(b[off:], c.cfg.MaxBufferSize)
		off += written
		if wasEmpty && written > 0 {
			c.notifyWriter()
		}
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

// Read returns de-shaped data (dummy chaff frames stripped), draining all
// already-buffered shaping frames in one call to minimize lock/copy overhead.
func (c *Conn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()

	total := 0
	if c.readOff < len(c.readBuf) {
		n := copy(b, c.readBuf[c.readOff:])
		c.readOff += n
		if c.readOff == len(c.readBuf) {
			c.readBuf = c.readBuf[:0]
			c.readOff = 0
		}
		total += n
		if total == len(b) {
			return total, nil
		}
	}

	for total < len(b) {
		if total > 0 {
			// Non-blocking batch drain: only read another shaping frame if its
			// complete header + body are already buffered in c.innerReader.
			if c.innerReader.Buffered() < outerHeaderLen {
				return total, nil
			}
			peekHdr, pErr := c.innerReader.Peek(outerHeaderLen)
			if pErr != nil {
				return total, nil
			}
			nextBodyLen := int(binary.BigEndian.Uint16(peekHdr[1:3]))
			if c.innerReader.Buffered() < outerHeaderLen+nextBodyLen {
				return total, nil
			}
		}

		if _, err := io.ReadFull(c.innerReader, c.readHdr[:]); err != nil {
			if total > 0 {
				return total, nil
			}
			return 0, err
		}

		frameType := c.readHdr[0]
		bodyLen := int(binary.BigEndian.Uint16(c.readHdr[1:3]))

		if cap(c.readFrame) < bodyLen {
			c.readFrame = make([]byte, bodyLen)
		}
		payload := c.readFrame[:bodyLen]
		if bodyLen > 0 {
			if _, err := io.ReadFull(c.innerReader, payload); err != nil {
				if total > 0 {
					return total, nil
				}
				return 0, err
			}
		}

		switch frameType {
		case frameTypeDummy:
			continue
		case frameTypeData:
			if bodyLen < dataLenField {
				if total > 0 {
					return total, nil
				}
				return 0, errInvalidFrame
			}
			dataLen := int(binary.BigEndian.Uint16(payload[:dataLenField]))
			if dataLen > bodyLen-dataLenField {
				if total > 0 {
					return total, nil
				}
				return 0, errInvalidFrame
			}
			if dataLen == 0 {
				continue
			}

			data := payload[dataLenField : dataLenField+dataLen]
			n := copy(b[total:], data)
			total += n
			if n < len(data) {
				c.readOff = 0
				c.readBuf = append(c.readBuf[:0], data[n:]...)
				return total, nil
			}
		default:
			if total > 0 {
				return total, nil
			}
			return 0, errInvalidFrame
		}
	}

	return total, nil
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
			if !chaffTimer.Stop() {
				select {
				case <-chaffTimer.C:
				default:
				}
			}
			chaffTimer.Reset(randDuration(c.cfg.IdleChaffMinInterval, c.cfg.IdleChaffMaxInterval))
		}
	}

	var paceTimer *time.Timer
	defer func() {
		if paceTimer != nil {
			paceTimer.Stop()
		}
	}()

	wasActive := false
	for {
		hasMore, wrote, ok := c.sendDataBurst(payloadCap)
		if !ok {
			return
		}
		if wrote {
			wasActive = true
			if hasMore {
				if paceTimer == nil {
					paceTimer = time.NewTimer(c.cfg.Interval)
				} else {
					paceTimer.Reset(c.cfg.Interval)
				}
				select {
				case <-c.closed:
					return
				case <-paceTimer.C:
				}
				continue
			}
			// Check once more in case data arrived during inner.Write.
			if c.hasBufferedData() {
				continue
			}
		}

		// Transitioning to idle: clear cached write deadline and re-arm chaff timer once.
		if c.deadlineArmed {
			_ = c.inner.SetWriteDeadline(time.Time{})
			c.deadlineArmed = false
		}
		if wasActive {
			resetChaff()
			wasActive = false
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
	n := c.writeRing.len
	c.writeMu.Unlock()
	return n > 0
}

func (c *Conn) sendDataBurst(payloadCap int) (hasMore bool, wrote bool, ok bool) {
	c.writeMu.Lock()
	buffered := c.writeRing.len
	if buffered == 0 {
		c.writeMu.Unlock()
		return false, false, true
	}

	slots := (buffered + payloadCap - 1) / payloadCap
	if slots > c.cfg.BurstSlots {
		slots = c.cfg.BurstSlots
	}

	maxBurstCap := c.cfg.BurstSlots * c.cfg.FrameSize
	if cap(c.burstBuf) < maxBurstCap {
		c.burstBuf = make([]byte, 0, maxBurstCap)
	}
	c.burstBuf = c.burstBuf[:0]
	for i := 0; i < slots; i++ {
		n := c.writeRing.len
		if n == 0 {
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

		start := len(c.burstBuf)
		if cap(c.burstBuf) < start+totalLen {
			grown := make([]byte, start+totalLen, max(start+totalLen, maxBurstCap))
			copy(grown, c.burstBuf)
			c.burstBuf = grown
		}
		frame := c.burstBuf[start : start+totalLen]
		c.burstBuf = c.burstBuf[:start+totalLen]

		frame[0] = frameTypeData
		binary.BigEndian.PutUint16(frame[1:3], uint16(bodyLen))
		binary.BigEndian.PutUint16(frame[3:5], uint16(n))
		c.writeRing.readCopy(frame[5 : 5+n])
		if totalLen > minTotal {
			fillPseudoRandom(frame[minTotal:totalLen])
		}
	}
	hasMore = c.writeRing.len > 0
	if c.writeWaiters > 0 {
		c.writeCond.Broadcast()
	}
	c.writeMu.Unlock()

	timeout := c.cfg.Interval * time.Duration(slots+1)
	if timeout < defaultWriteTimeout {
		timeout = defaultWriteTimeout
	}
	now := time.Now()
	if !c.deadlineArmed || now.After(c.deadlineRefresh) {
		_ = c.inner.SetWriteDeadline(now.Add(timeout + time.Second))
		c.deadlineArmed = true
		c.deadlineRefresh = now.Add(time.Second)
	}

	buf := c.burstBuf
	for len(buf) > 0 {
		select {
		case <-c.closed:
			_ = c.inner.SetWriteDeadline(time.Time{})
			c.deadlineArmed = false
			return false, true, false
		default:
		}
		chunk := len(buf)
		if chunk > maxCoalesceWrite {
			chunk = maxCoalesceWrite
		}
		if _, err := c.inner.Write(buf[:chunk]); err != nil {
			_ = c.inner.SetWriteDeadline(time.Time{})
			c.deadlineArmed = false
			c.failClose()
			return false, true, false
		}
		buf = buf[chunk:]
	}

	return hasMore, true, true
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
		if cap(c.burstBuf) < totalLen {
			c.burstBuf = make([]byte, totalLen)
		}
		frame := c.burstBuf[:totalLen]
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

func (c *Conn) SetDeadline(t time.Time) error     { return c.inner.SetReadDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error { return c.inner.SetReadDeadline(t) }

// SetWriteDeadline is a no-op because writeLoop manages its own per-burst write
// timeouts on c.inner; letting Session reset the write deadline after queueing
// a frame would clear writeLoop's active deadline concurrently.
func (c *Conn) SetWriteDeadline(t time.Time) error { return nil }

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
