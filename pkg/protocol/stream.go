package protocol

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Stream represents a single multiplexed proxy connection within a Session.
type Stream struct {
	id       uint32
	session  *Session
	priority uint8

	// Read side: incoming data from remote
	readBuf     chan []byte
	readBufOnce sync.Once   // ensures readBuf is closed exactly once
	doneCh      chan struct{} // closed when stream is done, unblocks deliverData
	readLeft    []byte        // leftover from partial reads

	// State
	closed   atomic.Bool
	closeMu  sync.Mutex
	closeErr error

	// Flow tracking
	bytesRead    atomic.Int64
	bytesWritten atomic.Int64
}

// newStream creates a new stream within a session.
func newStream(id uint32, session *Session) *Stream {
	return &Stream{
		id:       id,
		session:  session,
		priority: DefaultStreamPriority,
		readBuf:  make(chan []byte, 1024), // large buffer to prevent event loop stalls
		doneCh:   make(chan struct{}),
	}
}

// closeReadBuf safely closes the readBuf channel exactly once.
// It may be called concurrently from Stream.Close, closeByRemote, or Session.Close.
func (s *Stream) closeReadBuf() {
	s.readBufOnce.Do(func() {
		close(s.doneCh)
		close(s.readBuf)
	})
}

// ID returns the stream identifier.
func (s *Stream) ID() uint32 {
	return s.id
}

// Read reads data from the stream, implementing io.Reader.
func (s *Stream) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrStreamClosed
	}

	// Use leftover data from previous read first
	if len(s.readLeft) > 0 {
		n := copy(p, s.readLeft)
		s.readLeft = s.readLeft[n:]
		s.bytesRead.Add(int64(n))
		return n, nil
	}

	// Wait for new data
	data, ok := <-s.readBuf
	if !ok {
		return 0, io.EOF
	}

	n := copy(p, data)
	if n < len(data) {
		s.readLeft = data[n:]
	}
	s.bytesRead.Add(int64(n))
	return n, nil
}

// Write writes data to the stream by sending cmdPSH frames, implementing io.Writer.
// Frames are batched and written under a single lock acquisition to reduce contention.
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrStreamClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	// Build all frames first, then write under a single lock
	var frames []*Frame
	for off := 0; off < len(p); {
		n := len(p) - off
		if n > MaxFrameDataLen {
			n = MaxFrameDataLen
		}
		frames = append(frames, NewFrame(CmdPSH, s.id, p[off:off+n]))
		off += n
	}

	if err := s.session.writeFrames(frames); err != nil {
		return 0, err
	}

	s.bytesWritten.Add(int64(len(p)))
	return len(p), nil
}

// Close closes the stream by sending cmdFIN.
func (s *Stream) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.closed.Load() {
		return nil
	}
	s.closed.Store(true)

	// Send FIN to remote (best effort)
	_ = s.session.writeFrame(NewFrame(CmdFIN, s.id, nil))

	// Close the read channel
	s.closeReadBuf()

	// Unregister from session
	s.session.removeStream(s.id)

	return nil
}

// deliverData pushes incoming data into the stream's read buffer.
// Called by the session event loop when a cmdPSH frame is received.
// Uses a fast non-blocking path with a bounded timeout fallback to
// prevent a single slow stream from stalling the entire session event loop.
func (s *Stream) deliverData(data []byte) error {
	// Fast path: non-blocking send
	select {
	case s.readBuf <- data:
		return nil
	case <-s.doneCh:
		return ErrStreamClosed
	default:
	}

	// Slow path: buffer full, wait with timeout to protect other streams
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case s.readBuf <- data:
		return nil
	case <-s.doneCh:
		return ErrStreamClosed
	case <-timer.C:
		// Consumer too slow — drop data to unblock the event loop
		return ErrStreamClosed
	}
}

// closeByRemote is called when the remote sends cmdFIN for this stream.
func (s *Stream) closeByRemote() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.closed.Load() {
		return
	}
	s.closed.Store(true)
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
