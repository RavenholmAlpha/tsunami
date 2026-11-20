package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultStreamWindow is the default per-stream sliding flow-control window (2 MiB).
	DefaultStreamWindow = 2 << 20
	// DefaultMaxStreamRecvBytes is the hard safety cap for buffered receive data on a single stream (4 MiB).
	DefaultMaxStreamRecvBytes = 4 << 20

	minStreamWindow = 64 << 10
	maxStreamWindow = 8 << 20
	maxWriteBatch   = 256 << 10
)

type recvChunk struct {
	data    []byte
	poolBuf *[]byte
}

// ClampStreamWindow bounds an advertised per-stream receive window into a safe range.
func ClampStreamWindow(window int) int {
	if window <= 0 {
		return DefaultStreamWindow
	}
	if window < minStreamWindow {
		return minStreamWindow
	}
	if window > maxStreamWindow {
		return maxStreamWindow
	}
	return window
}

// Stream represents a single multiplexed proxy connection within a Session.
type Stream struct {
	id       uint32
	session  *Session
	priority uint8

	// Receive side: non-blocking bounded ring queue so RunEventLoop never stalls on a slow consumer.
	recvMu        sync.Mutex
	recvRing      []recvChunk
	recvHead      int
	recvCount     int
	recvBytes     int
	maxRecvBytes  int
	recvEOF       bool
	recvErr       error
	readNotify    chan struct{}
	recvLeftChunk recvChunk // leftover from partial reads

	// Open / close lifecycle.
	doneCh      chan struct{} // closed when stream is done, unblocks reads and writers
	openResult  chan error    // resolved by SYNACK for locally initiated streams
	openOnce    sync.Once
	readBufOnce sync.Once

	// Per-stream sliding-window flow control (when negotiated).
	flowControl           atomic.Bool
	sendMu                sync.Mutex
	sendWindow            int64
	sendWinNotify         chan struct{}
	unackedRead           atomic.Int64
	windowUpdateThreshold atomic.Int64

	// State.
	closed      atomic.Bool
	localClosed atomic.Bool
	closeMu     sync.Mutex

	// Flow tracking.
	bytesRead    atomic.Int64
	bytesWritten atomic.Int64
}

// newStream creates a new stream within a session.
func newStream(id uint32, session *Session) *Stream {
	return &Stream{
		id:            id,
		session:       session,
		priority:      DefaultStreamPriority,
		maxRecvBytes:  DefaultMaxStreamRecvBytes,
		readNotify:    make(chan struct{}, 1),
		doneCh:        make(chan struct{}),
		openResult:    make(chan error, 1),
		sendWinNotify: make(chan struct{}, 1),
	}
}

func (s *Stream) initFlowControl(localWindow, remoteWindow int) {
	if localWindow <= 0 {
		localWindow = DefaultStreamWindow
	}
	if remoteWindow <= 0 {
		remoteWindow = DefaultStreamWindow
	}
	s.sendMu.Lock()
	s.sendWindow = int64(remoteWindow)
	s.flowControl.Store(true)
	s.sendMu.Unlock()

	thresh := int64(localWindow) / 4
	if thresh < 1 {
		thresh = 1
	}
	s.windowUpdateThreshold.Store(thresh)

	capLimit := localWindow * 2
	if capLimit < DefaultMaxStreamRecvBytes {
		capLimit = DefaultMaxStreamRecvBytes
	}
	s.recvMu.Lock()
	s.maxRecvBytes = capLimit
	s.recvMu.Unlock()
}

func (s *Stream) setMaxRecvBytes(limit int) {
	s.recvMu.Lock()
	s.maxRecvBytes = limit
	s.recvMu.Unlock()
}

func (s *Stream) waitForOpen(timeout time.Duration) error {
	// Prefer an already delivered SYNACK result over doneCh. Rejection closes
	// the stream as part of cleanup, but callers still need the rejection cause.
	select {
	case err := <-s.openResult:
		return err
	default:
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-s.openResult:
		return err
	case <-timer.C:
		return fmt.Errorf("tsunami: wait for stream %d SYNACK: %w", s.id, ErrSessionClosed)
	case <-s.doneCh:
		select {
		case err := <-s.openResult:
			return err
		default:
		}
		return ErrStreamClosed
	}
}

func (s *Stream) resolveOpen(err error) {
	s.openOnce.Do(func() {
		s.openResult <- err
	})
}

// closeReadBuf safely marks the stream's done channel closed exactly once.
// It may be called concurrently from Stream.Close, closeByRemote, or Session.Close.
func (s *Stream) closeReadBuf() {
	s.readBufOnce.Do(func() {
		close(s.doneCh)
	})
}

// ID returns the stream identifier.
func (s *Stream) ID() uint32 {
	return s.id
}

// Read reads data from the stream, implementing io.Reader.
func (s *Stream) Read(p []byte) (int, error) {
	if len(s.recvLeftChunk.data) > 0 {
		n := copy(p, s.recvLeftChunk.data)
		s.recvLeftChunk.data = s.recvLeftChunk.data[n:]
		if len(s.recvLeftChunk.data) == 0 && s.recvLeftChunk.poolBuf != nil {
			putRecvPayloadBuf(s.recvLeftChunk.poolBuf)
			s.recvLeftChunk.poolBuf = nil
		}
		s.bytesRead.Add(int64(n))
		s.maybeSendWindowUpdate(n)
		return n, nil
	}

	for {
		s.recvMu.Lock()
		if s.localClosed.Load() {
			s.recvMu.Unlock()
			return 0, ErrStreamClosed
		}
		if s.recvCount > 0 {
			chunk := s.popRecvLocked()
			s.recvBytes -= len(chunk.data)
			s.recvMu.Unlock()

			n := s.consumeReadData(p, chunk)
			s.maybeSendWindowUpdate(n)
			return n, nil
		}
		if s.recvErr != nil {
			err := s.recvErr
			s.recvMu.Unlock()
			return 0, err
		}
		if s.recvEOF || s.closed.Load() {
			s.recvMu.Unlock()
			return 0, io.EOF
		}
		s.recvMu.Unlock()

		select {
		case <-s.readNotify:
		case <-s.doneCh:
		}
	}
}

func (s *Stream) pushRecvLocked(ch recvChunk) {
	if s.recvCount == len(s.recvRing) {
		newCap := len(s.recvRing) * 2
		if newCap < 16 {
			newCap = 16
		}
		newRing := make([]recvChunk, newCap)
		for i := 0; i < s.recvCount; i++ {
			newRing[i] = s.recvRing[(s.recvHead+i)%len(s.recvRing)]
		}
		s.recvRing = newRing
		s.recvHead = 0
	}
	tail := (s.recvHead + s.recvCount) % len(s.recvRing)
	s.recvRing[tail] = ch
	s.recvCount++
}

func (s *Stream) popRecvLocked() recvChunk {
	ch := s.recvRing[s.recvHead]
	s.recvRing[s.recvHead] = recvChunk{}
	s.recvHead = (s.recvHead + 1) % len(s.recvRing)
	s.recvCount--
	if s.recvCount == 0 {
		s.recvHead = 0
	}
	return ch
}

func (s *Stream) consumeReadData(p []byte, chunk recvChunk) int {
	n := copy(p, chunk.data)
	if n < len(chunk.data) {
		s.recvLeftChunk = recvChunk{data: chunk.data[n:], poolBuf: chunk.poolBuf}
	} else if chunk.poolBuf != nil {
		putRecvPayloadBuf(chunk.poolBuf)
	}
	s.bytesRead.Add(int64(n))
	return n
}

func (s *Stream) maybeSendWindowUpdate(consumed int) {
	if consumed <= 0 || s.closed.Load() || !s.flowControl.Load() {
		return
	}

	cur := s.unackedRead.Add(int64(consumed))
	if cur < s.windowUpdateThreshold.Load() {
		return
	}
	delta := s.unackedRead.Swap(0)
	if delta <= 0 {
		return
	}

	for delta > 0 {
		step := delta
		if step > 0xFFFFFFFF {
			step = 0xFFFFFFFF
		}
		var payload [4]byte
		binary.BigEndian.PutUint32(payload[:], uint32(step))
		if err := s.session.writeFrame(NewFrame(CmdWindowUpdate, s.id, payload[:])); err != nil {
			return
		}
		delta -= step
	}
}

func (s *Stream) addSendWindow(delta int64) {
	if delta <= 0 {
		return
	}
	s.sendMu.Lock()
	s.sendWindow += delta
	s.sendMu.Unlock()

	select {
	case s.sendWinNotify <- struct{}{}:
	default:
	}
}

func (s *Stream) acquireSendWindow(want int) (enabled bool, grant int) {
	if !s.flowControl.Load() {
		return false, want
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendWindow <= 0 {
		return true, 0
	}
	g := int64(want)
	if g > s.sendWindow {
		g = s.sendWindow
	}
	if g > maxWriteBatch {
		g = maxWriteBatch
	}
	s.sendWindow -= g
	return true, int(g)
}

// Write writes data to the stream by sending cmdPSH frames, implementing io.Writer.
// When flow control is negotiated, Write respects the peer's per-stream send window
// without blocking other streams on the session.
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrStreamClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	direct := s.session.canWritePSHDirect()
	fc, grant := s.acquireSendWindow(len(p))
	if !fc {
		// Legacy / flow-control-disabled path: batch all frames in a single write.
		var err error
		if direct {
			err = s.session.writePSHData(s.id, p)
		} else {
			err = s.session.writeFrames(buildPSHFrames(s.id, p))
		}
		if err != nil {
			return 0, err
		}
		s.bytesWritten.Add(int64(len(p)))
		return len(p), nil
	}

	off := 0
	for off < len(p) {
		if grant == 0 {
			if s.closed.Load() {
				return off, ErrStreamClosed
			}
			select {
			case <-s.sendWinNotify:
			case <-s.doneCh:
				return off, ErrStreamClosed
			}
			_, grant = s.acquireSendWindow(len(p) - off)
			continue
		}

		chunk := p[off : off+grant]
		var err error
		if direct {
			err = s.session.writePSHData(s.id, chunk)
		} else {
			err = s.session.writeFrames(buildPSHFrames(s.id, chunk))
		}
		if err != nil {
			return off, err
		}
		off += grant
		s.bytesWritten.Add(int64(grant))
		if off < len(p) {
			_, grant = s.acquireSendWindow(len(p) - off)
		}
	}

	return off, nil
}

func buildPSHFrames(streamID uint32, p []byte) []*Frame {
	var frames []*Frame
	for off := 0; off < len(p); {
		n := len(p) - off
		if n > MaxFrameDataLen {
			n = MaxFrameDataLen
		}
		frames = append(frames, NewFrame(CmdPSH, streamID, p[off:off+n]))
		off += n
	}
	return frames
}

// Close closes the stream by sending cmdFIN.
func (s *Stream) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.closed.Load() {
		return nil
	}
	s.localClosed.Store(true)
	s.closed.Store(true)
	s.resolveOpen(ErrStreamClosed)

	s.recvMu.Lock()
	s.recvEOF = true
	for s.recvCount > 0 {
		ch := s.popRecvLocked()
		putRecvPayloadBuf(ch.poolBuf)
	}
	s.recvRing = nil
	s.recvHead = 0
	s.recvCount = 0
	s.recvBytes = 0
	s.recvMu.Unlock()

	// Send FIN to remote (best effort).
	_ = s.session.writeFrame(NewFrame(CmdFIN, s.id, nil))

	s.closeReadBuf()
	s.session.removeStream(s.id)

	return nil
}

// deliverData pushes incoming data into the stream's non-blocking receive queue.
// It never blocks the session's RunEventLoop; if the per-stream hard buffer cap
// is exceeded, it returns ErrStreamRecvOverflow so only this stream is closed.
func (s *Stream) deliverData(data []byte) error {
	return s.deliverDataPooled(data, nil)
}

func (s *Stream) deliverDataPooled(data []byte, poolBuf *[]byte) error {
	if s.closed.Load() {
		putRecvPayloadBuf(poolBuf)
		return ErrStreamClosed
	}

	s.recvMu.Lock()
	if s.recvEOF || s.localClosed.Load() || s.closed.Load() {
		s.recvMu.Unlock()
		putRecvPayloadBuf(poolBuf)
		return ErrStreamClosed
	}
	if len(data) == 0 {
		s.recvMu.Unlock()
		putRecvPayloadBuf(poolBuf)
		return nil
	}
	if s.maxRecvBytes > 0 && s.recvBytes+len(data) > s.maxRecvBytes {
		s.recvMu.Unlock()
		putRecvPayloadBuf(poolBuf)
		return ErrStreamRecvOverflow
	}
	s.pushRecvLocked(recvChunk{data: data, poolBuf: poolBuf})
	s.recvBytes += len(data)
	s.recvMu.Unlock()

	select {
	case s.readNotify <- struct{}{}:
	default:
	}
	return nil
}

// closeByRemote is called when the remote sends cmdFIN or the stream is aborted.
func (s *Stream) closeByRemote(err error) {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.closed.Load() {
		return
	}
	s.closed.Store(true)
	resolveErr := err
	if resolveErr == nil {
		resolveErr = ErrStreamClosed
	}
	s.resolveOpen(resolveErr)

	s.recvMu.Lock()
	s.recvEOF = true
	if err != nil && err != ErrStreamClosed && s.recvErr == nil {
		s.recvErr = err
	}
	s.recvMu.Unlock()

	s.closeReadBuf()
	s.session.removeStream(s.id)
}

// SetPriority sets the stream's scheduling priority (0=highest, 255=lowest).
func (s *Stream) SetPriority(priority uint8) {
	s.priority = priority
}

// Priority returns the stream's scheduling priority.
func (s *Stream) Priority() uint8 {
	return s.priority
}

// BytesRead returns the total number of bytes read from this stream.
func (s *Stream) BytesRead() int64 {
	return s.bytesRead.Load()
}

// BytesWritten returns the total number of bytes written to this stream.
func (s *Stream) BytesWritten() int64 {
	return s.bytesWritten.Load()
}
