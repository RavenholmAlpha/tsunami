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
	// DefaultStreamWindow is the default per-stream sliding flow-control window (8 MiB).
	DefaultStreamWindow = 8 << 20
	// DefaultMaxStreamRecvBytes is the hard safety cap for buffered receive data on a single stream (16 MiB).
	DefaultMaxStreamRecvBytes = 16 << 20

	minStreamWindow = 64 << 10
	maxStreamWindow = 16 << 20
	maxWriteBatch   = 4 * AlignedRelayPayloadLen
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
// Ready chunks in recvRing are coalesced in a single non-blocking pass.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		if s.localClosed.Load() {
			return 0, ErrStreamClosed
		}
		return 0, nil
	}

	total := 0
	if len(s.recvLeftChunk.data) > 0 {
		n := copy(p, s.recvLeftChunk.data)
		s.recvLeftChunk.data = s.recvLeftChunk.data[n:]
		if len(s.recvLeftChunk.data) == 0 && s.recvLeftChunk.poolBuf != nil {
			putRecvPayloadBuf(s.recvLeftChunk.poolBuf)
			s.recvLeftChunk.poolBuf = nil
		}
		total += n
		if total == len(p) {
			s.bytesRead.Add(int64(total))
			s.maybeSendWindowUpdate(total)
			return total, nil
		}
	}

	for {
		s.recvMu.Lock()
		if s.localClosed.Load() {
			s.recvMu.Unlock()
			if total > 0 {
				s.bytesRead.Add(int64(total))
				s.maybeSendWindowUpdate(total)
				return total, nil
			}
			return 0, ErrStreamClosed
		}
		var popped [16]recvChunk
		poppedCount := 0
		want := len(p) - total
		for poppedCount < len(popped) && want > 0 && s.recvCount > 0 {
			chunk := s.popRecvLocked()
			s.recvBytes -= len(chunk.data)
			popped[poppedCount] = chunk
			poppedCount++
			want -= len(chunk.data)
		}
		if poppedCount > 0 || total > 0 {
			s.recvMu.Unlock()
			for i := 0; i < poppedCount; i++ {
				chunk := popped[i]
				n := copy(p[total:], chunk.data)
				total += n
				if n < len(chunk.data) {
					s.recvLeftChunk = recvChunk{data: chunk.data[n:], poolBuf: chunk.poolBuf}
				} else if chunk.poolBuf != nil {
					putRecvPayloadBuf(chunk.poolBuf)
				}
			}
			s.bytesRead.Add(int64(total))
			s.maybeSendWindowUpdate(total)
			return total, nil
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
		s.session.enqueueControl(ctrlFrame{
			cmd:      CmdWindowUpdate,
			streamID: s.id,
			winDelta: uint32(step),
		})
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

// WritePreframedSingle writes a single CmdPSH frame whose 7-byte header space is
// already contiguous in front of the payload (len(framed) == FrameHeaderLen+payloadLen),
// avoiding a 64 KiB payload copy once the stream's handshake padding epoch finishes.
func (s *Stream) WritePreframedSingle(framed []byte, payloadLen int) error {
	if s.closed.Load() {
		return ErrStreamClosed
	}
	if payloadLen <= 0 {
		return nil
	}
	if payloadLen > MaxFrameDataLen || len(framed) != FrameHeaderLen+payloadLen || !s.session.canWritePSHDirect() {
		_, err := s.Write(framed[FrameHeaderLen:])
		return err
	}

	fc, grant := s.acquireSendWindow(payloadLen)
	for fc && grant == 0 {
		if s.closed.Load() {
			return ErrStreamClosed
		}
		select {
		case <-s.sendWinNotify:
		case <-s.doneCh:
			return ErrStreamClosed
		}
		_, grant = s.acquireSendWindow(payloadLen)
	}
	if fc && grant < payloadLen {
		if grant > 0 {
			s.addSendWindow(int64(grant))
		}
		_, err := s.Write(framed[FrameHeaderLen:])
		return err
	}

	framed[0] = byte(CmdPSH)
	binary.BigEndian.PutUint32(framed[1:5], s.id)
	binary.BigEndian.PutUint16(framed[5:7], uint16(payloadLen))
	if err := s.session.writePSHPreframed(s.id, framed, payloadLen); err != nil {
		return err
	}
	s.bytesWritten.Add(int64(payloadLen))
	return nil
}

// CopyToStream copies from src into dst using a pre-framed 7B+65529B buffer so
// steady-state relay writes avoid an extra 64 KiB payload copy and align cleanly
// to 4×16 KiB TLS records without emitting a 6-byte trailing runt TLS record.
func CopyToStream(dst *Stream, src io.Reader) (int64, error) {
	bufp := PreframedRelayBufPool.Get().(*[]byte)
	buf := *bufp
	payloadBuf := buf[FrameHeaderLen : FrameHeaderLen+AlignedRelayPayloadLen]
	var written int64
	var err error
	for {
		nr, er := src.Read(payloadBuf)
		if nr > 0 {
			if ew := dst.WritePreframedSingle(buf[:FrameHeaderLen+nr], nr); ew != nil {
				err = ew
				break
			}
			written += int64(nr)
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	*bufp = buf[:FrameHeaderLen+MaxFrameDataLen]
	PreframedRelayBufPool.Put(bufp)
	return written, err
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

func (s *Stream) drainRecvPool() {
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
}

// Close closes the stream by sending cmdFIN.
func (s *Stream) Close() error {
	s.closeMu.Lock()
	if s.closed.Load() {
		s.closeMu.Unlock()
		return nil
	}
	s.localClosed.Store(true)
	s.closed.Store(true)
	s.resolveOpen(ErrStreamClosed)
	s.drainRecvPool()
	s.closeReadBuf()
	s.closeMu.Unlock()

	// Unregister stream slot immediately and queue FIN asynchronously so Close
	// never blocks on outbound write backpressure.
	s.session.removeStream(s.id)
	s.session.enqueueControl(ctrlFrame{cmd: CmdFIN, streamID: s.id})

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
	if s.closed.Load() {
		s.closeMu.Unlock()
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
	s.closeMu.Unlock()

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
