package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Session represents a single TLS connection carrying multiplexed streams.
type Session struct {
	seq     uint64
	conn    io.ReadWriteCloser
	writer  *FrameWriter
	writeMu sync.Mutex // protects writer from concurrent writes

	// Stream management
	streams            map[uint32]*Stream
	streamMu           sync.RWMutex
	openMu             sync.Mutex // preserves on-wire SYN ordering for concurrent OpenStream calls
	nextID             atomic.Uint32
	activeStreams      atomic.Int32
	lastRemoteStreamID uint32 // accessed only by the session event loop
	maxStreams         int    // 0 disables the limit

	// Session state
	closed    atomic.Bool
	closeMu   sync.Mutex
	closeOnce sync.Once
	doneCh    chan struct{}

	// Negotiated settings
	localVersion       int
	remoteVersion      int
	flowControl        atomic.Bool
	fastOpen           atomic.Bool
	localStreamWindow  int
	remoteStreamWindow int

	// Write-side activity & stall tracking (cache-line separated from reader atomics).
	idleSince    atomic.Int64 // stores UnixNano; 0 = not idle
	lastActive   atomic.Int64 // stores UnixNano
	writingSince atomic.Int64 // stores UnixNano while blocked in conn.Write; 0 = idle
	_            [40]byte

	// Read-side activity tracking.
	lastRecv atomic.Int64 // stores UnixNano; updated on any inbound frame
	lastPong atomic.Int64 // stores UnixNano; updated on CmdHeartResponse

	// Callbacks
	onStreamOpen          func(s *Stream)         // Called when server receives a new stream
	onStreamCountChange   func(activeCount int)   // Called when active stream count changes
	onPaddingSchemeUpdate func(data []byte) error // Called when client receives a padding scheme update
	onStreamClosed        func(id uint32)         // Called after a stream is removed from the session
	streamClosedMu        sync.Mutex              // protects onStreamClosed

	// Optional padding-aware write functions.
	// When set, frame writes route through these instead of the raw FrameWriter.
	// Callbacks are invoked under writeMu and must not call writeFrame.
	paddingWriteFn          func(frames []*Frame) error
	paddingPSHWriteFn       func(streamID uint32, data []byte) error
	paddingPSHPreframedFn   func(streamID uint32, framed []byte, payloadLen int) (bool, error)
	paddingSYNTargetWriteFn func(streamID uint32, addrData []byte) error
	paddingCoalesce         atomic.Pointer[paddingCoalesceFuncs]
	hasCustomWriteFn        atomic.Bool
	hasPSHWriteFn           atomic.Bool

	// Write priority gate: prevents bulk stream writes from starving control frames
	// (CmdWindowUpdate, CmdSYN, CmdSYNACK, CmdFIN, heartbeats) and small interactive PSH.
	highPriWaiters atomic.Int32
	bulkWaiting    atomic.Int32
	highPriMu      sync.Mutex
	highPriCond    *sync.Cond

	// Contention-coalescing batch writer for steady-state unpadded bulk CmdPSH.
	// Uses pooled 256 KiB buffers so idle sessions pin zero batch memory.
	batchMu       sync.Mutex
	batchBusy     bool
	batchBusyFast atomic.Bool
	batchBufp     *[]byte
	batchBuf      []byte
	batchWaiter   *writeBatchWaiter

	// Non-blocking outbound control queue (CmdSYNACK, CmdWindowUpdate, CmdFIN,
	// CmdHeartResponse) so RunEventLoop and Stream.Read never block on writeMu.
	ctrlCh        chan ctrlFrame
	ctrlStartOnce sync.Once

	// Reusable scratch arrays for frame dispatch under writeMu (avoids per-call heap escapes).
	ctrlFrames  [16]Frame
	ctrlPtrs    [16]*Frame
	ctrlWinBufs [16][4]byte

	// Error channel
	errCh chan error
}

type ctrlFrame struct {
	cmd      Command
	streamID uint32
	winDelta uint32
	data     []byte
}

type paddingCoalesceFuncs struct {
	canCoalesce func(streamID uint32) bool
	writeRaw    func(data []byte) error
}

type writeBatchWaiter struct {
	done chan struct{}
	err  error
}

const (
	sessionWriteDeadline   = 30 * time.Second
	highPriPSHMaxBytes     = 2048
	maxCoalesceBatchBytes  = 256 << 10
	batchCoalescePoolCap   = maxCoalesceBatchBytes + 1024
	maxCoalesceDrainRounds = 4
	sessionReadBufSize     = 128 * 1024
)

var batchCoalesceBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, batchCoalescePoolCap)
		return &buf
	},
}

// DefaultMaxStreams bounds how many streams one session may carry at once.
// It is deliberately far above real browsing concurrency but low enough that a
// SYN flood cannot exhaust memory or goroutines; callers can override it with
// SetMaxStreams.
const DefaultMaxStreams = 256

// NewSession creates a new session over the given connection.
func NewSession(conn io.ReadWriteCloser, seq uint64) *Session {
	s := &Session{
		seq:          seq,
		conn:         conn,
		writer:       NewFrameWriter(conn),
		streams:      make(map[uint32]*Stream),
		doneCh:       make(chan struct{}),
		localVersion: CurrentVersion,
		maxStreams:   DefaultMaxStreams,
		ctrlCh:       make(chan ctrlFrame, 256),
		errCh:        make(chan error, 1),
	}
	s.highPriCond = sync.NewCond(&s.highPriMu)
	now := time.Now().UnixNano()
	s.lastActive.Store(now)
	s.lastRecv.Store(now)
	s.lastPong.Store(now)
	return s
}

// SetMaxStreams changes the per-session stream limit. n <= 0 disables the
// limit. It should be called before the session starts processing streams.
func (s *Session) SetMaxStreams(n int) {
	s.streamMu.Lock()
	s.maxStreams = n
	s.streamMu.Unlock()
}

// MaxStreams returns the current per-session stream limit (0 = unlimited).
func (s *Session) MaxStreams() int {
	s.streamMu.RLock()
	defer s.streamMu.RUnlock()
	return s.maxStreams
}

// streamCount returns the number of currently tracked streams.
func (s *Session) streamCount() int {
	return int(s.activeStreams.Load())
}

// ReplaceConn swaps the underlying connection for a wrapped one (e.g., traffic shaping).
// Must be called before RunEventLoop.
func (s *Session) ReplaceConn(conn io.ReadWriteCloser) {
	s.conn = conn
	s.writer = NewFrameWriter(conn)
}

// Seq returns the session sequence number (monotonically increasing within a client).
func (s *Session) Seq() uint64 {
	return s.seq
}

// EnableFlowControl turns on per-stream sliding-window flow control for
// subsequent streams opened on this session. Must be called before RunEventLoop.
func (s *Session) EnableFlowControl(localWindow, remoteWindow int) {
	if localWindow <= 0 {
		localWindow = DefaultStreamWindow
	}
	if remoteWindow <= 0 {
		remoteWindow = DefaultStreamWindow
	}
	s.streamMu.Lock()
	s.localStreamWindow = localWindow
	s.remoteStreamWindow = remoteWindow
	s.flowControl.Store(true)
	s.streamMu.Unlock()
}

// EnableFastOpen enables 0-RTT stream creation where OpenStream sends CmdSYN
// and returns immediately without waiting for CmdSYNACK.
func (s *Session) EnableFastOpen() {
	s.fastOpen.Store(true)
}

// OpenStream creates a new stream on this session (client side).
func (s *Session) OpenStream() (*Stream, error) {
	s.openMu.Lock()
	if s.closed.Load() {
		s.openMu.Unlock()
		return nil, ErrSessionClosed
	}
	if limit := s.MaxStreams(); limit > 0 && s.streamCount() >= limit {
		s.openMu.Unlock()
		return nil, ErrTooManyStreams
	}

	id := s.nextID.Add(1)
	stream := newStream(id, s)

	s.streamMu.Lock()
	if s.flowControl.Load() {
		stream.initFlowControl(s.localStreamWindow, s.remoteStreamWindow)
	}
	s.streams[id] = stream
	count := len(s.streams)
	s.activeStreams.Store(int32(count))
	s.streamMu.Unlock()

	s.notifyStreamCountChange(count)

	// Send SYN (stack ctrlFrame avoids heap-allocating a *Frame per OpenStream).
	var syn [1]ctrlFrame
	syn[0] = ctrlFrame{cmd: CmdSYN, streamID: id}
	if err := s.writeControlBatch(syn[:]); err != nil {
		s.openMu.Unlock()
		stream.closeByRemote(err)
		return nil, fmt.Errorf("tsunami: send SYN: %w", err)
	}
	s.openMu.Unlock()

	if !s.fastOpen.Load() {
		if err := stream.waitForOpen(10 * time.Second); err != nil {
			_ = stream.Close()
			return nil, err
		}
	}

	s.lastActive.Store(time.Now().UnixNano())
	return stream, nil
}

// OpenStreamWithTarget opens a stream and sends its initial target-address payload.
// When FastOpen is active, CmdSYN and the target address are coalesced into a
// single lock acquisition and single padded record on the wire.
func (s *Session) OpenStreamWithTarget(addrData []byte) (*Stream, error) {
	if len(addrData) == 0 || len(addrData) > MaxFrameDataLen || !s.fastOpen.Load() {
		stream, err := s.OpenStream()
		if err != nil {
			return nil, err
		}
		if len(addrData) > 0 {
			if _, err := stream.Write(addrData); err != nil {
				_ = stream.Close()
				return nil, err
			}
		}
		return stream, nil
	}

	s.openMu.Lock()
	if s.closed.Load() {
		s.openMu.Unlock()
		return nil, ErrSessionClosed
	}
	if limit := s.MaxStreams(); limit > 0 && s.streamCount() >= limit {
		s.openMu.Unlock()
		return nil, ErrTooManyStreams
	}

	id := s.nextID.Add(1)
	stream := newStream(id, s)

	s.streamMu.Lock()
	if s.flowControl.Load() {
		stream.initFlowControl(s.localStreamWindow, s.remoteStreamWindow)
		stream.sendWindow -= int64(len(addrData))
	}
	s.streams[id] = stream
	count := len(s.streams)
	s.activeStreams.Store(int32(count))
	s.streamMu.Unlock()

	s.notifyStreamCountChange(count)

	if err := s.writeSYNWithTarget(id, addrData); err != nil {
		s.openMu.Unlock()
		stream.closeByRemote(err)
		return nil, fmt.Errorf("tsunami: send SYN+target: %w", err)
	}
	s.openMu.Unlock()

	stream.bytesWritten.Add(int64(len(addrData)))
	s.lastActive.Store(time.Now().UnixNano())
	return stream, nil
}

// ActiveStreamCount returns the number of active streams.
func (s *Session) ActiveStreamCount() int {
	return int(s.activeStreams.Load())
}

// IsIdle returns true if the session has no active streams.
func (s *Session) IsIdle() bool {
	return s.ActiveStreamCount() == 0
}

// IdleSince returns when the session became idle. Zero value if not idle.
func (s *Session) IdleSince() time.Time {
	ns := s.idleSince.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// SetIdleSince sets the idle start time.
func (s *Session) SetIdleSince(t time.Time) {
	if t.IsZero() {
		s.idleSince.Store(0)
	} else {
		s.idleSince.Store(t.UnixNano())
	}
}

// LastActive returns the time of last activity.
func (s *Session) LastActive() time.Time {
	ns := s.lastActive.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// LastRecv returns the time of the most recently received valid frame.
func (s *Session) LastRecv() time.Time {
	ns := s.lastRecv.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// LastPong returns the time of the last heartbeat response.
func (s *Session) LastPong() time.Time {
	ns := s.lastPong.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// IsAlive returns true if the session has responded to a heartbeat within the given timeout.
func (s *Session) IsAlive(timeout time.Duration) bool {
	if s.closed.Load() {
		return false
	}
	lp := s.LastPong()
	if lp.IsZero() {
		return true // no heartbeat sent yet, assume alive
	}
	return time.Since(lp) < timeout
}

func (s *Session) lockWriteHighPri() {
	s.highPriWaiters.Add(1)
	s.writeMu.Lock()
	if s.highPriWaiters.Add(-1) == 0 && s.bulkWaiting.Load() > 0 {
		s.highPriMu.Lock()
		s.highPriCond.Broadcast()
		s.highPriMu.Unlock()
	}
}

func (s *Session) unlockWriteHighPri() {
	s.writeMu.Unlock()
}

func (s *Session) waitForHighPri() {
	if s.highPriWaiters.Load() > 0 && !s.closed.Load() {
		s.highPriMu.Lock()
		s.bulkWaiting.Add(1)
		for s.highPriWaiters.Load() > 0 && !s.closed.Load() {
			s.highPriCond.Wait()
		}
		s.bulkWaiting.Add(-1)
		s.highPriMu.Unlock()
	}
}

func (s *Session) lockWriteBulk() {
	s.waitForHighPri()
	s.writeMu.Lock()
}

func (s *Session) unlockWriteBulk() {
	s.writeMu.Unlock()
}

func (s *Session) enqueueControl(cf ctrlFrame) {
	if s.closed.Load() {
		return
	}
	s.ctrlStartOnce.Do(func() {
		go s.runControlWriter()
	})
	select {
	case s.ctrlCh <- cf:
	default:
		go func(f ctrlFrame) {
			var one [1]ctrlFrame
			one[0] = f
			_ = s.writeControlBatch(one[:])
		}(cf)
	}
}

func (s *Session) runControlWriter() {
	var batch [16]ctrlFrame
	for {
		select {
		case <-s.doneCh:
			return
		case cf := <-s.ctrlCh:
			batch[0] = cf
			n := 1
			for n < len(batch) {
				select {
				case next := <-s.ctrlCh:
					batch[n] = next
					n++
				default:
					goto flush
				}
			}
		flush:
			_ = s.writeControlBatch(batch[:n])
		}
	}
}

func (s *Session) writeControlBatch(batch []ctrlFrame) error {
	if len(batch) == 0 || s.closed.Load() {
		return nil
	}
	s.lockWriteHighPri()
	if s.closed.Load() {
		s.unlockWriteHighPri()
		return ErrSessionClosed
	}
	n := len(batch)
	if n > len(s.ctrlFrames) {
		n = len(s.ctrlFrames)
	}
	for i := 0; i < n; i++ {
		cf := batch[i]
		data := cf.data
		if cf.cmd == CmdWindowUpdate {
			binary.BigEndian.PutUint32(s.ctrlWinBufs[i][:], cf.winDelta)
			data = s.ctrlWinBufs[i][:]
		}
		s.ctrlFrames[i] = Frame{
			Command:  cf.cmd,
			StreamID: cf.streamID,
			Data:     data,
		}
		s.ctrlPtrs[i] = &s.ctrlFrames[i]
	}
	now := time.Now().UnixNano()
	s.lastActive.Store(now)
	s.writingSince.Store(now)
	var err error
	if s.paddingWriteFn != nil {
		err = s.paddingWriteFn(s.ctrlPtrs[:n])
	} else if n == 1 {
		err = s.writer.WriteFrame(s.ctrlPtrs[0])
	} else {
		err = s.writer.WriteFrames(s.ctrlPtrs[:n])
	}
	for i := 0; i < n; i++ {
		s.ctrlFrames[i].Data = nil
		s.ctrlPtrs[i] = nil
	}
	s.writingSince.Store(0)
	s.unlockWriteHighPri()
	if err != nil && !s.closed.Load() {
		go func() { _ = s.Close() }()
	}
	return err
}

// appendPendingControlLocked piggybacks already-queued control frames onto an
// outbound steady-state bulk CmdPSH batch so control traffic avoids standalone
// ctl-padded TLS records during active transfers while preserving full ctl
// padding when the session is idle.
func (s *Session) appendPendingControlLocked(buf []byte) []byte {
	var hdr [FrameHeaderLen]byte
	var win [4]byte
	for i := 0; i < 32; i++ {
		if cap(buf)-len(buf) < FrameHeaderLen+4 {
			return buf
		}
		select {
		case cf := <-s.ctrlCh:
			dataLen := len(cf.data)
			if cf.cmd == CmdWindowUpdate {
				dataLen = 4
			}
			if len(buf)+FrameHeaderLen+dataLen > cap(buf) {
				// Re-queue or send asynchronously if oversized error payload doesn't fit.
				go func(f ctrlFrame) {
					var one [1]ctrlFrame
					one[0] = f
					_ = s.writeControlBatch(one[:])
				}(cf)
				return buf
			}
			hdr[0] = byte(cf.cmd)
			binary.BigEndian.PutUint32(hdr[1:5], cf.streamID)
			binary.BigEndian.PutUint16(hdr[5:7], uint16(dataLen))
			buf = append(buf, hdr[:]...)
			if cf.cmd == CmdWindowUpdate {
				binary.BigEndian.PutUint32(win[:], cf.winDelta)
				buf = append(buf, win[:]...)
			} else if dataLen > 0 {
				buf = append(buf, cf.data...)
			}
		default:
			return buf
		}
	}
	return buf
}

func (s *Session) canSessionCoalesce() bool {
	if s.paddingCoalesce.Load() != nil {
		return true
	}
	return !s.hasCustomWriteFn.Load() && !s.hasPSHWriteFn.Load()
}

func (s *Session) canCoalescePSH(streamID uint32) bool {
	if streamID == 0 {
		return false
	}
	if fns := s.paddingCoalesce.Load(); fns != nil {
		return fns.canCoalesce(streamID)
	}
	return !s.hasCustomWriteFn.Load() && !s.hasPSHWriteFn.Load()
}

func (s *Session) writeRawCoalescedLocked(data []byte) error {
	if fns := s.paddingCoalesce.Load(); fns != nil {
		return fns.writeRaw(data)
	}
	_, err := s.writer.w.Write(data)
	return err
}

func (s *Session) tryEnqueueBulkPSH(streamID uint32, framed []byte, payload []byte) (bool, error) {
	if !s.batchBusyFast.Load() || s.highPriWaiters.Load() > 0 || s.closed.Load() {
		return false, nil
	}
	var wireSize int
	if framed != nil {
		wireSize = len(framed)
	} else {
		chunks := (len(payload) + MaxFrameDataLen - 1) / MaxFrameDataLen
		wireSize = chunks*FrameHeaderLen + len(payload)
	}
	if wireSize <= 0 || wireSize > maxCoalesceBatchBytes {
		return false, nil
	}
	if !s.canCoalescePSH(streamID) {
		return false, nil
	}

	s.batchMu.Lock()
	if !s.batchBusy || s.highPriWaiters.Load() > 0 || s.closed.Load() || len(s.batchBuf)+wireSize > maxCoalesceBatchBytes {
		s.batchMu.Unlock()
		return false, nil
	}
	if s.batchBufp == nil {
		s.batchBufp = batchCoalesceBufPool.Get().(*[]byte)
		s.batchBuf = (*s.batchBufp)[:0]
	}
	if framed != nil {
		s.batchBuf = append(s.batchBuf, framed...)
	} else {
		var hdr [FrameHeaderLen]byte
		for off := 0; off < len(payload); {
			n := len(payload) - off
			if n > MaxFrameDataLen {
				n = MaxFrameDataLen
			}
			hdr[0] = byte(CmdPSH)
			binary.BigEndian.PutUint32(hdr[1:5], streamID)
			binary.BigEndian.PutUint16(hdr[5:7], uint16(n))
			s.batchBuf = append(s.batchBuf, hdr[:]...)
			s.batchBuf = append(s.batchBuf, payload[off:off+n]...)
			off += n
		}
	}
	waiter := s.batchWaiter
	if waiter == nil {
		waiter = &writeBatchWaiter{done: make(chan struct{})}
		s.batchWaiter = waiter
	}
	s.batchMu.Unlock()

	<-waiter.done
	return true, waiter.err
}

func (s *Session) acquireBulkOrCoalesce(streamID uint32, framed []byte, payload []byte) (bool, error) {
	if ok, err := s.tryEnqueueBulkPSH(streamID, framed, payload); ok {
		return true, err
	}
	s.waitForHighPri()
	if s.closed.Load() {
		return true, ErrSessionClosed
	}
	if ok, err := s.tryEnqueueBulkPSH(streamID, framed, payload); ok {
		return true, err
	}
	if s.writeMu.TryLock() {
		return false, nil
	}
	if ok, err := s.tryEnqueueBulkPSH(streamID, framed, payload); ok {
		return true, err
	}
	s.writeMu.Lock()
	return false, nil
}

func (s *Session) markBatchBusy() {
	s.batchMu.Lock()
	s.batchBusy = true
	s.batchBusyFast.Store(true)
	s.batchMu.Unlock()
}

func (s *Session) drainCoalescedBatches(prevErr error) {
	for rounds := 0; ; rounds++ {
		s.batchMu.Lock()
		if len(s.batchBuf) == 0 {
			s.batchBusy = false
			s.batchBusyFast.Store(false)
			if s.batchBufp != nil {
				if cap(s.batchBuf) == batchCoalescePoolCap {
					*s.batchBufp = s.batchBuf[:0]
					batchCoalesceBufPool.Put(s.batchBufp)
				}
				s.batchBufp = nil
				s.batchBuf = nil
			}
			s.batchMu.Unlock()
			return
		}
		toFlushBufp := s.batchBufp
		toFlush := s.batchBuf
		waiter := s.batchWaiter
		s.batchBufp = nil
		s.batchBuf = nil
		s.batchWaiter = nil

		stopAfterThis := prevErr != nil ||
			s.closed.Load() ||
			s.highPriWaiters.Load() > 0 ||
			rounds >= maxCoalesceDrainRounds-1
		if stopAfterThis {
			s.batchBusy = false
			s.batchBusyFast.Store(false)
		}
		s.batchMu.Unlock()

		var flushErr error
		if prevErr != nil {
			flushErr = prevErr
		} else if s.closed.Load() {
			flushErr = ErrSessionClosed
		} else {
			toFlush = s.appendPendingControlLocked(toFlush)
			now := time.Now().UnixNano()
			s.lastActive.Store(now)
			s.writingSince.Store(now)
			flushErr = s.writeRawCoalescedLocked(toFlush)
			s.writingSince.Store(0)
			if flushErr != nil {
				prevErr = flushErr
				if !s.closed.Load() {
					go func() { _ = s.Close() }()
				}
			}
		}

		waiter.err = flushErr
		close(waiter.done)

		if toFlushBufp != nil && cap(toFlush) == batchCoalescePoolCap {
			*toFlushBufp = toFlush[:0]
			batchCoalesceBufPool.Put(toFlushBufp)
		}

		if stopAfterThis {
			return
		}
	}
}

// writeFrame writes a frame to the underlying TLS connection.
// It is safe for concurrent use by multiple goroutines.
// If a paddingWriteFn is set, frames are routed through it for padding.
func (s *Session) writeFrame(f *Frame) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if f == nil {
		return ErrInvalidFrame
	}
	if len(f.Data) > MaxFrameDataLen {
		return ErrFrameTooLarge
	}
	s.lockWriteHighPri()
	if s.closed.Load() {
		s.unlockWriteHighPri()
		return ErrSessionClosed
	}
	now := time.Now().UnixNano()
	// Idle keepalive CmdWaste frames must not reset lastActive, otherwise a
	// blackholed TCP socket would keep postponing real CmdHeartRequest probes.
	if f.Command != CmdWaste {
		s.lastActive.Store(now)
	}
	s.writingSince.Store(now)
	var err error
	if s.paddingWriteFn != nil {
		s.ctrlPtrs[0] = f
		err = s.paddingWriteFn(s.ctrlPtrs[:1])
		s.ctrlPtrs[0] = nil
	} else {
		err = s.writer.WriteFrame(f)
	}
	s.writingSince.Store(0)
	s.unlockWriteHighPri()
	if err != nil && !s.closed.Load() {
		go func() { _ = s.Close() }()
	}
	return err
}

// WriteFrame writes a single frame through the session's write path.
// It is safe for concurrent use by multiple goroutines.
func (s *Session) WriteFrame(f *Frame) error {
	return s.writeFrame(f)
}

// writeFrames writes multiple frames under a single lock acquisition.
// This reduces lock contention when a stream needs to send large payloads
// that span multiple frames.
func (s *Session) writeFrames(frames []*Frame) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	for _, frame := range frames {
		if frame == nil {
			return ErrInvalidFrame
		}
		if len(frame.Data) > MaxFrameDataLen {
			return ErrFrameTooLarge
		}
	}
	s.lockWriteBulk()
	if s.closed.Load() {
		s.unlockWriteBulk()
		return ErrSessionClosed
	}
	now := time.Now().UnixNano()
	s.lastActive.Store(now)
	s.writingSince.Store(now)
	var err error
	if s.paddingWriteFn != nil {
		err = s.paddingWriteFn(frames)
	} else {
		err = s.writer.WriteFrames(frames)
	}
	s.writingSince.Store(0)
	s.unlockWriteBulk()
	if err != nil && !s.closed.Load() {
		go func() { _ = s.Close() }()
	}
	return err
}

func (s *Session) canWritePSHDirect() bool {
	return !s.hasCustomWriteFn.Load() || s.hasPSHWriteFn.Load()
}

func (s *Session) writePSHData(streamID uint32, data []byte) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) <= highPriPSHMaxBytes {
		s.lockWriteHighPri()
		if s.closed.Load() {
			s.unlockWriteHighPri()
			return ErrSessionClosed
		}
		now := time.Now().UnixNano()
		s.lastActive.Store(now)
		s.writingSince.Store(now)
		var err error
		if s.paddingPSHWriteFn != nil {
			err = s.paddingPSHWriteFn(streamID, data)
		} else {
			err = s.writeRawPSHLocked(streamID, data)
		}
		s.writingSince.Store(0)
		s.unlockWriteHighPri()
		if err != nil && !s.closed.Load() {
			go func() { _ = s.Close() }()
		}
		return err
	}

	if coalesced, err := s.acquireBulkOrCoalesce(streamID, nil, data); coalesced {
		return err
	}
	if s.closed.Load() {
		s.unlockWriteBulk()
		return ErrSessionClosed
	}
	coalesce := s.canSessionCoalesce()
	if coalesce {
		s.markBatchBusy()
	}
	now := time.Now().UnixNano()
	s.lastActive.Store(now)
	s.writingSince.Store(now)
	var err error
	if s.paddingPSHWriteFn != nil {
		err = s.paddingPSHWriteFn(streamID, data)
	} else {
		err = s.writeRawPSHLocked(streamID, data)
	}
	s.writingSince.Store(0)
	if coalesce {
		s.drainCoalescedBatches(err)
	}
	s.unlockWriteBulk()
	if err != nil && !s.closed.Load() {
		go func() { _ = s.Close() }()
	}
	return err
}

func (s *Session) writePSHPreframed(streamID uint32, framed []byte, payloadLen int) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if payloadLen <= highPriPSHMaxBytes {
		s.lockWriteHighPri()
		if s.closed.Load() {
			s.unlockWriteHighPri()
			return ErrSessionClosed
		}
		now := time.Now().UnixNano()
		s.lastActive.Store(now)
		s.writingSince.Store(now)
		err := s.writePSHPreframedLocked(streamID, framed, payloadLen)
		s.writingSince.Store(0)
		s.unlockWriteHighPri()
		if err != nil && !s.closed.Load() {
			go func() { _ = s.Close() }()
		}
		return err
	}

	if coalesced, err := s.acquireBulkOrCoalesce(streamID, framed, nil); coalesced {
		return err
	}
	if s.closed.Load() {
		s.unlockWriteBulk()
		return ErrSessionClosed
	}
	coalesce := s.canSessionCoalesce()
	if coalesce {
		s.markBatchBusy()
	}
	now := time.Now().UnixNano()
	s.lastActive.Store(now)
	s.writingSince.Store(now)
	err := s.writePSHPreframedLocked(streamID, framed, payloadLen)
	s.writingSince.Store(0)
	if coalesce {
		s.drainCoalescedBatches(err)
	}
	s.unlockWriteBulk()
	if err != nil && !s.closed.Load() {
		go func() { _ = s.Close() }()
	}
	return err
}

func (s *Session) writePSHPreframedLocked(streamID uint32, framed []byte, payloadLen int) error {
	if s.paddingPSHPreframedFn != nil {
		handled, err := s.paddingPSHPreframedFn(streamID, framed, payloadLen)
		if !handled && err == nil {
			payload := framed[FrameHeaderLen:]
			if s.paddingPSHWriteFn != nil {
				return s.paddingPSHWriteFn(streamID, payload)
			}
			return s.writeRawPSHLocked(streamID, payload)
		}
		return err
	}
	if s.paddingPSHWriteFn != nil {
		return s.paddingPSHWriteFn(streamID, framed[FrameHeaderLen:])
	}
	if s.paddingWriteFn != nil {
		s.ctrlFrames[0] = Frame{Command: CmdPSH, StreamID: streamID, Data: framed[FrameHeaderLen:]}
		s.ctrlPtrs[0] = &s.ctrlFrames[0]
		err := s.paddingWriteFn(s.ctrlPtrs[:1])
		s.ctrlFrames[0].Data = nil
		s.ctrlPtrs[0] = nil
		return err
	}
	_, err := s.writer.w.Write(framed)
	return err
}

func (s *Session) writeSYNWithTarget(streamID uint32, addrData []byte) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	s.lockWriteHighPri()
	if s.closed.Load() {
		s.unlockWriteHighPri()
		return ErrSessionClosed
	}
	now := time.Now().UnixNano()
	s.lastActive.Store(now)
	s.writingSince.Store(now)
	var err error
	if s.paddingSYNTargetWriteFn != nil {
		err = s.paddingSYNTargetWriteFn(streamID, addrData)
	} else {
		s.ctrlFrames[0] = Frame{Command: CmdSYN, StreamID: streamID}
		s.ctrlFrames[1] = Frame{Command: CmdPSH, StreamID: streamID, Data: addrData}
		s.ctrlPtrs[0] = &s.ctrlFrames[0]
		s.ctrlPtrs[1] = &s.ctrlFrames[1]
		if s.paddingWriteFn != nil {
			err = s.paddingWriteFn(s.ctrlPtrs[:2])
		} else {
			err = s.writer.WriteFrames(s.ctrlPtrs[:2])
		}
		s.ctrlFrames[1].Data = nil
		s.ctrlPtrs[0] = nil
		s.ctrlPtrs[1] = nil
	}
	s.writingSince.Store(0)
	s.unlockWriteHighPri()
	if err != nil && !s.closed.Load() {
		go func() { _ = s.Close() }()
	}
	return err
}

func (s *Session) writeRawPSHLocked(streamID uint32, data []byte) error {
	if FrameHeaderLen+len(data) <= len(s.writer.smallBuf) {
		s.writer.smallBuf[0] = byte(CmdPSH)
		binary.BigEndian.PutUint32(s.writer.smallBuf[1:5], streamID)
		binary.BigEndian.PutUint16(s.writer.smallBuf[5:7], uint16(len(data)))
		copy(s.writer.smallBuf[FrameHeaderLen:], data)
		_, err := s.writer.w.Write(s.writer.smallBuf[:FrameHeaderLen+len(data)])
		return err
	}
	if len(data) <= MaxFrameDataLen {
		poolBufp := frameBufPool.Get().(*[]byte)
		buf := (*poolBufp)[:FrameHeaderLen+len(data)]
		buf[0] = byte(CmdPSH)
		binary.BigEndian.PutUint32(buf[1:5], streamID)
		binary.BigEndian.PutUint16(buf[5:7], uint16(len(data)))
		copy(buf[FrameHeaderLen:], data)
		_, err := s.writer.w.Write(buf)
		*poolBufp = (*poolBufp)[:MaxFrameDataLen+FrameHeaderLen]
		frameBufPool.Put(poolBufp)
		return err
	}

	chunks := (len(data) + MaxFrameDataLen - 1) / MaxFrameDataLen
	totalLen := chunks*FrameHeaderLen + len(data)
	bufp := writeCoalesceBufPool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < totalLen {
		buf = make([]byte, 0, totalLen)
	}
	buf = buf[:0]
	var header [FrameHeaderLen]byte
	for off := 0; off < len(data); {
		n := len(data) - off
		if n > MaxFrameDataLen {
			n = MaxFrameDataLen
		}
		header[0] = byte(CmdPSH)
		binary.BigEndian.PutUint32(header[1:5], streamID)
		binary.BigEndian.PutUint16(header[5:7], uint16(n))
		buf = append(buf, header[:]...)
		buf = append(buf, data[off:off+n]...)
		off += n
	}
	_, err := s.writer.w.Write(buf)
	if cap(buf) <= maxCoalescePool {
		*bufp = buf[:0]
		writeCoalesceBufPool.Put(bufp)
	}
	return err
}

// removeStream unregisters a stream from the session.
func (s *Session) removeStream(id uint32) {
	s.streamMu.Lock()
	delete(s.streams, id)
	count := len(s.streams)
	s.activeStreams.Store(int32(count))
	s.streamMu.Unlock()

	s.notifyStreamCountChange(count)

	if count == 0 {
		s.idleSince.Store(time.Now().UnixNano())
	}

	// Run the callback after streamMu is released. The padding writer uses this
	// hook to forget per-stream state; it must never run under the stream lock.
	s.notifyStreamClosed(id)
}

// getStream returns the stream with the given ID, or nil.
func (s *Session) getStream(id uint32) *Stream {
	s.streamMu.RLock()
	defer s.streamMu.RUnlock()
	return s.streams[id]
}

// SetOnStreamOpen sets the callback for new incoming streams (server side).
func (s *Session) SetOnStreamOpen(fn func(*Stream)) {
	s.onStreamOpen = fn
}

// SetOnStreamCountChange sets the callback invoked when active stream count changes.
// This is used to drive keepalive active/idle state transitions.
func (s *Session) SetOnStreamCountChange(fn func(activeCount int)) {
	s.onStreamCountChange = fn
}

// SetOnStreamClosed sets the callback invoked after a stream has been removed
// from the session (local Close or remote FIN). It is used by the padding layer
// to forget per-stream padding state. The callback runs after streamMu is
// released, so it may call back into the session without deadlocking.
func (s *Session) SetOnStreamClosed(fn func(id uint32)) {
	s.streamClosedMu.Lock()
	s.onStreamClosed = fn
	s.streamClosedMu.Unlock()
}

// notifyStreamClosed invokes the stream-closed callback outside of streamMu.
func (s *Session) notifyStreamClosed(id uint32) {
	s.streamClosedMu.Lock()
	fn := s.onStreamClosed
	s.streamClosedMu.Unlock()
	if fn != nil {
		fn(id)
	}
}

// SetOnPaddingSchemeUpdate sets the callback invoked when the remote pushes a
// new padding scheme. The callback receives the raw scheme text.
func (s *Session) SetOnPaddingSchemeUpdate(fn func(data []byte) error) {
	s.onPaddingSchemeUpdate = fn
}

// SetPaddingWriteFn sets a custom write function that applies padding.
// When set, all frame writes go through this function instead of the raw FrameWriter.
// The function is called under the writeMu lock — it must not call writeFrame.
func (s *Session) SetPaddingWriteFn(fn func(frames []*Frame) error) {
	s.writeMu.Lock()
	s.paddingWriteFn = fn
	s.hasCustomWriteFn.Store(fn != nil)
	s.writeMu.Unlock()
}

// SetPaddingPSHWriteFn sets a fast-path callback for writing raw stream payload
// bytes without intermediate Frame allocations.
func (s *Session) SetPaddingPSHWriteFn(fn func(streamID uint32, data []byte) error) {
	s.writeMu.Lock()
	s.paddingPSHWriteFn = fn
	s.hasPSHWriteFn.Store(fn != nil)
	s.writeMu.Unlock()
}

// SetPaddingPSHPreframedWriteFn sets an optional zero-copy callback for single
// CmdPSH frames that already have 7 bytes of header space reserved in-place.
func (s *Session) SetPaddingPSHPreframedWriteFn(fn func(streamID uint32, framed []byte, payloadLen int) (bool, error)) {
	s.writeMu.Lock()
	s.paddingPSHPreframedFn = fn
	s.writeMu.Unlock()
}

// SetPaddingSYNTargetWriteFn sets an optional callback to coalesce CmdSYN + target
// address into a single padded record while preserving stream padding epochs.
func (s *Session) SetPaddingSYNTargetWriteFn(fn func(streamID uint32, addrData []byte) error) {
	s.writeMu.Lock()
	s.paddingSYNTargetWriteFn = fn
	s.writeMu.Unlock()
}

// SetPaddingCoalesceFns wires the steady-state padding epoch predicate and raw
// write callback so multi-stream bulk CmdPSH frames past their handshake epoch
// can coalesce into a single write under contention without altering padding.
func (s *Session) SetPaddingCoalesceFns(canFn func(streamID uint32) bool, rawFn func(data []byte) error) {
	s.writeMu.Lock()
	if canFn != nil && rawFn != nil {
		s.paddingCoalesce.Store(&paddingCoalesceFuncs{canCoalesce: canFn, writeRaw: rawFn})
	} else {
		s.paddingCoalesce.Store(nil)
	}
	s.writeMu.Unlock()
}

// notifyStreamCountChange calls the stream count change callback if set.
func (s *Session) notifyStreamCountChange(count int) {
	if s.onStreamCountChange != nil {
		s.onStreamCountChange(count)
	}
}

// RunEventLoop runs the session event loop, reading and dispatching frames.
// This blocks until the session is closed or an error occurs.
func (s *Session) RunEventLoop() error {
	defer s.Close()

	stopWatch := make(chan struct{})
	go s.runWriteWatchdog(stopWatch)
	defer close(stopWatch)

	reader := NewBufferedFrameReader(s.conn, sessionReadBufSize)
	for {
		if s.closed.Load() {
			return nil
		}

		frame, poolBuf, err := reader.readFramePooled()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return fmt.Errorf("tsunami: read frame: %w", err)
		}

		now := time.Now().UnixNano()
		s.lastActive.Store(now)
		s.lastRecv.Store(now)

		if err := s.handleFramePooled(frame, poolBuf); err != nil {
			return err
		}
	}
}

func (s *Session) runWriteWatchdog(stopCh <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-s.doneCh:
			return
		case <-ticker.C:
			ws := s.writingSince.Load()
			if ws != 0 && time.Now().UnixNano()-ws > int64(sessionWriteDeadline) {
				if nc, ok := s.conn.(net.Conn); ok {
					_ = nc.SetWriteDeadline(time.Unix(1, 0))
				}
				_ = s.Close()
				return
			}
		}
	}
}

// handleFrame dispatches a received frame.
func (s *Session) handleFrame(f *Frame) error {
	return s.handleFramePooled(f, nil)
}

func (s *Session) handleFramePooled(f *Frame, poolBuf *[]byte) error {
	switch f.Command {
	case CmdWaste:
		// Silently discard padding data
		return nil

	case CmdSYN:
		if s.onStreamOpen == nil {
			return ErrInvalidFrame
		}
		if f.StreamID == 0 || f.StreamID <= s.lastRemoteStreamID || s.getStream(f.StreamID) != nil {
			s.enqueueControl(ctrlFrame{cmd: CmdSYNACK, streamID: f.StreamID, data: []byte("invalid stream id")})
			return nil
		}
		if limit := s.MaxStreams(); limit > 0 && s.streamCount() >= limit {
			s.enqueueControl(ctrlFrame{cmd: CmdSYNACK, streamID: f.StreamID, data: []byte("too many streams")})
			return nil
		}

		// Server-side: new stream opened by client.
		stream := newStream(f.StreamID, s)
		s.streamMu.Lock()
		if s.flowControl.Load() {
			stream.initFlowControl(s.localStreamWindow, s.remoteStreamWindow)
		}
		s.streams[f.StreamID] = stream
		count := len(s.streams)
		s.activeStreams.Store(int32(count))
		s.streamMu.Unlock()
		s.lastRemoteStreamID = f.StreamID
		s.notifyStreamCountChange(count)
		if s.onStreamOpen != nil {
			go s.onStreamOpen(stream)
		}
		// Queue SYNACK asynchronously so RunEventLoop never stalls on outbound
		// write backpressure or deadlocks with the peer's event loop.
		s.enqueueControl(ctrlFrame{cmd: CmdSYNACK, streamID: f.StreamID})

	case CmdSYNACK:
		// Client-side: stream confirmed by server
		stream := s.getStream(f.StreamID)
		if stream == nil {
			return nil // ignore for unknown streams
		}
		if len(f.Data) > 0 {
			// Error response — close the stream
			log.Printf("tsunami: stream %d rejected: %s", f.StreamID, string(f.Data))
			stream.closeByRemote(fmt.Errorf("%w: %s", ErrStreamRejected, string(f.Data)))
			return nil
		}
		stream.resolveOpen(nil)

	case CmdPSH:
		stream := s.getStream(f.StreamID)
		if stream == nil {
			putRecvPayloadBuf(poolBuf)
			return nil // ignore data for closed/unknown streams
		}
		if err := stream.deliverDataPooled(f.Data, poolBuf); err != nil {
			if errors.Is(err, ErrStreamRecvOverflow) {
				log.Printf("tsunami: stream %d receive buffer overflow; closing stream", f.StreamID)
				stream.closeByRemote(ErrStreamRecvOverflow)
				s.enqueueControl(ctrlFrame{cmd: CmdFIN, streamID: f.StreamID})
			}
			return nil
		}

	case CmdWindowUpdate:
		stream := s.getStream(f.StreamID)
		if stream == nil || len(f.Data) != 4 {
			return nil
		}
		delta := binary.BigEndian.Uint32(f.Data)
		if delta > 0 {
			stream.addSendWindow(int64(delta))
		}
		return nil

	case CmdFIN:
		stream := s.getStream(f.StreamID)
		if stream != nil {
			stream.closeByRemote(nil)
		}

	case CmdHeartRequest:
		s.enqueueControl(ctrlFrame{cmd: CmdHeartResponse})
		return nil

	case CmdHeartResponse:
		// Received pong — reuse the timestamp already captured on frame arrival.
		s.lastPong.Store(s.lastRecv.Load())
		return nil

	case CmdAlert:
		log.Printf("tsunami: server alert: %s", string(f.Data))
		return ErrSessionClosed

	case CmdSettings:
		// Server receives client settings (handled by server logic above session)
		return nil

	case CmdServerSettings:
		// Client receives server settings
		if f.Data != nil {
			settings, err := DecodeServerSettings(f.Data)
			if err == nil {
				s.remoteVersion = settings.Version
			}
		}

	case CmdUpdatePaddingScheme:
		if s.onPaddingSchemeUpdate != nil {
			return s.onPaddingSchemeUpdate(f.Data)
		}
		return nil

	case CmdSurgeCtrl, CmdBandwidthReport, CmdStreamPriority:
		// TSUNAMI v3 extensions — handled by upper layer
		return nil

	default:
		// Unknown command — silently ignore for forward compatibility
		return nil
	}

	return nil
}

// SendSettings sends client settings as the first frame of the session.
func (s *Session) SendSettings(settings *ClientSettings) error {
	data := EncodeClientSettings(settings)
	return s.writeFrame(NewFrame(CmdSettings, 0, data))
}

// SendServerSettings sends server settings in response to client settings.
func (s *Session) SendServerSettings(settings *ServerSettings) error {
	data := EncodeServerSettings(settings)
	return s.writeFrame(NewFrame(CmdServerSettings, 0, data))
}

// SendAlert sends an ALERT message and initiates session close.
func (s *Session) SendAlert(msg string) error {
	return s.writeFrame(NewFrame(CmdAlert, 0, []byte(msg)))
}

// SendHeartbeat sends a heartbeat request.
func (s *Session) SendHeartbeat() error {
	return s.writeFrame(NewFrame(CmdHeartRequest, 0, nil))
}

// Close closes the session and all its streams.
func (s *Session) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	var err error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.highPriMu.Lock()
		s.highPriCond.Broadcast()
		s.highPriMu.Unlock()
		close(s.doneCh)

		// Close all streams and return any queued pooled receive buffers.
		s.streamMu.Lock()
		for _, stream := range s.streams {
			stream.closed.Store(true)
			stream.resolveOpen(ErrSessionClosed)
			stream.drainRecvPool()
			stream.closeReadBuf()
		}
		s.streams = make(map[uint32]*Stream)
		s.activeStreams.Store(0)
		s.streamMu.Unlock()

		if s.writingSince.Load() != 0 {
			if nc, ok := s.conn.(net.Conn); ok {
				_ = nc.SetWriteDeadline(time.Unix(1, 0))
			}
		}
		// Close underlying connection
		err = s.conn.Close()
	})
	return err
}

// IsClosed returns true if the session has been closed.
func (s *Session) IsClosed() bool {
	return s.closed.Load()
}
