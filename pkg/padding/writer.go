package padding

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// Writer wraps a TLS connection writer and applies the PaddingScheme
// to outgoing data. It tracks the TLS Write counter and applies
// segment-splitting and padding per the active scheme.
type Writer struct {
	w         io.Writer
	scheme    *Scheme
	mu        sync.Mutex
	packetIdx int // current TLS Write counter
	fw        *protocol.FrameWriter
}

// NewWriter creates a new padding-aware writer.
func NewWriter(w io.Writer, scheme *Scheme) *Writer {
	return &Writer{
		w:      w,
		scheme: scheme,
		fw:     protocol.NewFrameWriter(w),
	}
}

// UpdateScheme replaces the current padding scheme.
// Takes effect from the next new session, not the current one.
func (pw *Writer) UpdateScheme(scheme *Scheme) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.scheme = scheme
}

// WriteFramesWithPadding writes frames applying the PaddingScheme rules.
// This is called for each TLS Write operation. The method:
//  1. Serializes all frames into a byte buffer
//  2. Applies the PaddingScheme for the current packet index
//  3. Performs segment-splitting and cmdWaste padding as needed
//  4. Increments the packet counter
func (pw *Writer) WriteFramesWithPadding(frames []*protocol.Frame) error {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	// Serialize frames to raw bytes
	userData := serializeFrames(frames)

	segments := pw.scheme.GetSegments(pw.packetIdx)
	pw.packetIdx++

	// No padding rule for this packet -> send directly
	if segments == nil {
		_, err := pw.w.Write(userData)
		return err
	}

	// Apply segment-splitting with padding
	return pw.applySplitting(userData, segments)
}

// applySplitting applies the padding segment strategy.
func (pw *Writer) applySplitting(userData []byte, segments []Segment) error {
	offset := 0
	remaining := len(userData)

	for _, seg := range segments {
		if seg.IsCheck {
			// 'c' check: if user data is exhausted, stop sending
			if remaining <= 0 {
				return nil
			}
			continue
		}

		targetLen := RandomInRange(seg.MinSize, seg.MaxSize)

		if remaining >= targetLen {
			// Enough user data — send a chunk of user data
			_, err := pw.w.Write(userData[offset : offset+targetLen])
			if err != nil {
				return err
			}
			offset += targetLen
			remaining -= targetLen
		} else if remaining > 0 {
			// Not enough user data — send remaining user data + waste padding
			// Subtract FrameHeaderLen because the waste frame wire format adds its own header
			wasteSize := targetLen - remaining - protocol.FrameHeaderLen
			if wasteSize < 0 {
				wasteSize = 0
			}
			wasteFrame := protocol.NewWasteFrame(wasteSize)
			wasteBytes := serializeFrames([]*protocol.Frame{wasteFrame})

			// Combine: remaining user data + waste frame
			combined := make([]byte, remaining+len(wasteBytes))
			copy(combined, userData[offset:offset+remaining])
			copy(combined[remaining:], wasteBytes)

			_, err := pw.w.Write(combined)
			if err != nil {
				return err
			}
			offset += remaining
			remaining = 0
		} else {
			// No user data left — send pure waste padding
			wasteFrame := protocol.NewWasteFrame(targetLen - protocol.FrameHeaderLen)
			if err := pw.fw.WriteFrame(wasteFrame); err != nil {
				return err
			}
		}
	}

	// If user data still has remaining bytes after all segments, send directly
	if remaining > 0 {
		_, err := pw.w.Write(userData[offset:])
		return err
	}

	return nil
}

// WriteRaw writes raw bytes directly without padding (used after padding stop).
func (pw *Writer) WriteRaw(data []byte) error {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	_, err := pw.w.Write(data)
	return err
}

// PacketIndex returns the current TLS Write counter.
func (pw *Writer) PacketIndex() int {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.packetIdx
}

// serializeFrames converts frames to their wire format bytes.
func serializeFrames(frames []*protocol.Frame) []byte {
	size := 0
	for _, f := range frames {
		size += protocol.FrameHeaderLen + len(f.Data)
	}

	buf := make([]byte, 0, size)
	for _, f := range frames {
		header := make([]byte, protocol.FrameHeaderLen)
		header[0] = byte(f.Command)
		header[1] = byte(f.StreamID >> 24)
		header[2] = byte(f.StreamID >> 16)
		header[3] = byte(f.StreamID >> 8)
		header[4] = byte(f.StreamID)
		header[5] = byte(len(f.Data) >> 8)
		header[6] = byte(len(f.Data))
		buf = append(buf, header...)
		buf = append(buf, f.Data...)
	}

	return buf
}

// KeepaliveGenerator sends minimal waste packets during idle periods.
// Behaves like a TLS heartbeat: 30-60s intervals, 4-8 byte packets.
// Only active when all Streams in the Session are idle.
type KeepaliveGenerator struct {
	config   *KeepaliveConfig
	stopCh   chan struct{}
	once     sync.Once
	isActive atomic.Bool // tracks whether streams are active
	writeFn  func(f *protocol.Frame) error // session-safe write function
}

// NewKeepaliveGenerator creates a keepalive generator from the scheme's config.
// writeFn should route writes through the session's write path for thread safety.
func NewKeepaliveGenerator(config *KeepaliveConfig, writeFn func(f *protocol.Frame) error) *KeepaliveGenerator {
	return &KeepaliveGenerator{
		config:  config,
		stopCh:  make(chan struct{}),
		writeFn: writeFn,
	}
}

// Start begins generating keepalive packets in a background goroutine.
func (kg *KeepaliveGenerator) Start() {
	if kg.config == nil {
		return
	}
	go kg.run()
}

// Stop stops the keepalive generator.
func (kg *KeepaliveGenerator) Stop() {
	kg.once.Do(func() {
		close(kg.stopCh)
	})
}

// SetActive marks whether there are active streams.
// Keepalive packets are only sent when no streams are active.
func (kg *KeepaliveGenerator) SetActive(active bool) {
	kg.isActive.Store(active)
}

func (kg *KeepaliveGenerator) run() {
	for {
		intervalMs := RandomInRange(kg.config.IntervalMinMs, kg.config.IntervalMaxMs)

		select {
		case <-kg.stopCh:
			return
		case <-time.After(time.Duration(intervalMs) * time.Millisecond):
			// Only send keepalive when idle (no active streams)
			if kg.isActive.Load() {
				continue
			}
			size := RandomInRange(kg.config.SizeMin, kg.config.SizeMax)
			wasteFrame := protocol.NewWasteFrame(size)
			_ = kg.writeFn(wasteFrame)
		}
	}
}
