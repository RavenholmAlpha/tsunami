package protocol

import (
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Session represents a single TLS connection carrying multiplexed streams.
type Session struct {
	seq       uint64
	conn      io.ReadWriteCloser
	writer    *FrameWriter
	reader    *FrameReader

	// Stream management
	streams   map[uint32]*Stream
	streamMu  sync.RWMutex
	nextID    atomic.Uint32

	// Session state
	closed    atomic.Bool
	closeMu   sync.Mutex
	closeOnce sync.Once

	// Negotiated settings
	localVersion  int
	remoteVersion int

	// Idle tracking
	idleSince time.Time
	lastActive time.Time

	// Callbacks
	onStreamOpen func(s *Stream) // Called when server receives a new stream

	// Error channel
	errCh chan error
}

// NewSession creates a new session over the given connection.
func NewSession(conn io.ReadWriteCloser, seq uint64) *Session {
	s := &Session{
		seq:          seq,
		conn:         conn,
		writer:       NewFrameWriter(conn),
		reader:       NewFrameReader(conn),
		streams:      make(map[uint32]*Stream),
		localVersion: CurrentVersion,
		lastActive:   time.Now(),
		errCh:        make(chan error, 1),
	}
	return s
}

// Seq returns the session sequence number (monotonically increasing within a client).
func (s *Session) Seq() uint64 {
	return s.seq
}

// OpenStream creates a new stream on this session (client side).
func (s *Session) OpenStream() (*Stream, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}

	id := s.nextID.Add(1)
	stream := newStream(id, s)

	s.streamMu.Lock()
	s.streams[id] = stream
	s.streamMu.Unlock()

	// Send SYN
	if err := s.writeFrame(NewFrame(CmdSYN, id, nil)); err != nil {
		s.removeStream(id)
		return nil, fmt.Errorf("tsunami: send SYN: %w", err)
	}

	s.lastActive = time.Now()
	return stream, nil
}

// ActiveStreamCount returns the number of active streams.
func (s *Session) ActiveStreamCount() int {
	s.streamMu.RLock()
	defer s.streamMu.RUnlock()
	return len(s.streams)
}

// IsIdle returns true if the session has no active streams.
func (s *Session) IsIdle() bool {
	return s.ActiveStreamCount() == 0
}

// IdleSince returns when the session became idle. Zero value if not idle.
func (s *Session) IdleSince() time.Time {
	return s.idleSince
}

// SetIdleSince sets the idle start time.
func (s *Session) SetIdleSince(t time.Time) {
	s.idleSince = t
}

// LastActive returns the time of last activity.
func (s *Session) LastActive() time.Time {
	return s.lastActive
}

// writeFrame writes a frame to the underlying TLS connection.
func (s *Session) writeFrame(f *Frame) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	s.lastActive = time.Now()
	return s.writer.WriteFrame(f)
}

// removeStream unregisters a stream from the session.
func (s *Session) removeStream(id uint32) {
	s.streamMu.Lock()
	delete(s.streams, id)
	idle := len(s.streams) == 0
	s.streamMu.Unlock()

	if idle {
		s.idleSince = time.Now()
	}
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

// RunEventLoop runs the session event loop, reading and dispatching frames.
// This blocks until the session is closed or an error occurs.
func (s *Session) RunEventLoop() error {
	defer s.Close()

	for {
		if s.closed.Load() {
			return nil
		}

		frame, err := s.reader.ReadFrame()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return fmt.Errorf("tsunami: read frame: %w", err)
		}

		s.lastActive = time.Now()

		if err := s.handleFrame(frame); err != nil {
			return err
		}
	}
}

// handleFrame dispatches a received frame.
func (s *Session) handleFrame(f *Frame) error {
	switch f.Command {
	case CmdWaste:
		// Silently discard padding data
		return nil

	case CmdSYN:
		// Server-side: new stream opened by client
		stream := newStream(f.StreamID, s)
		s.streamMu.Lock()
		s.streams[f.StreamID] = stream
		s.streamMu.Unlock()
		if s.onStreamOpen != nil {
			go s.onStreamOpen(stream)
		}

	case CmdSYNACK:
		// Client-side: stream confirmed by server
		stream := s.getStream(f.StreamID)
		if stream == nil {
			return nil // ignore for unknown streams
		}
		if len(f.Data) > 0 {
			// Error response — close the stream
			log.Printf("tsunami: stream %d rejected: %s", f.StreamID, string(f.Data))
			stream.closeByRemote()
		}
		// Success — no action needed

	case CmdPSH:
		stream := s.getStream(f.StreamID)
		if stream == nil {
			return nil // ignore data for closed/unknown streams
		}
		if err := stream.deliverData(f.Data); err != nil {
			return nil // non-fatal: stream buffer full
		}

	case CmdFIN:
		stream := s.getStream(f.StreamID)
		if stream != nil {
			stream.closeByRemote()
		}

	case CmdHeartRequest:
		return s.writeFrame(NewFrame(CmdHeartResponse, 0, nil))

	case CmdHeartResponse:
		// Received pong — update last active
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
		// Client should update padding — handled by upper layer
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

		// Close all streams
		s.streamMu.Lock()
		for _, stream := range s.streams {
			stream.closed.Store(true)
			close(stream.readBuf)
		}
		s.streams = make(map[uint32]*Stream)
		s.streamMu.Unlock()

		// Close underlying connection
		err = s.conn.Close()
	})
	return err
}

// IsClosed returns true if the session has been closed.
func (s *Session) IsClosed() bool {
	return s.closed.Load()
}
