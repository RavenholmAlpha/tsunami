package padding

import (
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// maxTrackedStreams bounds the per-stream padding epoch table. Sessions are
// long-lived and can carry thousands of streams; when the table grows beyond
// this limit the oldest entry is pruned. Closed streams are normally removed
// through ForgetStream (wired to Session.SetOnStreamClosed), so pruning is
// only a safety net against callers that never report stream closes.
const maxTrackedStreams = 1024

// streamState is the padding epoch of a single data stream.
type streamState struct {
	idx        int       // number of data writes seen for this stream
	bytes      int64     // bytes written for this stream (payload + padding)
	insertedAt time.Time // used to prune the oldest entries
	seq        uint64    // tie-breaker when timestamps are equal
	ready      bool      // true once permanently past handshake padding under current scheme
}

// paddingEncodeBufPool pools temporary frame-serialization and padding assembly
// buffers across sessions so idle sessions pin zero large scratch memory while
// active sessions stay allocation-free.
var paddingEncodeBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 16*1024)
		return &buf
	},
}

func getPaddingBuf(minCap int) (*[]byte, []byte) {
	if minCap > maxReusableRawBuf {
		return nil, make([]byte, 0, minCap)
	}
	bufp := paddingEncodeBufPool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < minCap {
		buf = make([]byte, 0, minCap)
	}
	return bufp, buf[:0]
}

func putPaddingBuf(bufp *[]byte, buf []byte) {
	if bufp == nil {
		return
	}
	if cap(buf) <= maxReusableRawBuf {
		*bufp = buf[:0]
		paddingEncodeBufPool.Put(bufp)
	}
}

// Writer wraps a TLS connection writer and applies the PaddingScheme to
// outgoing data.
//
// Data writes are bucketed per stream: a call whose frames are all CmdPSH with
// the same non-zero StreamID advances that stream's epoch. Everything else is
// a control write and always uses the scheme's ctl rule.
type Writer struct {
	w      io.Writer
	scheme *Scheme
	role   Role
	fw     *protocol.FrameWriter

	mu       sync.Mutex
	ctlIdx   int   // control-bucket write counter (PacketIndex)
	ctlBytes int64 // bytes written for control writes (observability)
	streams  map[uint32]*streamState
	nextSeq  uint64

	// Lock-free / leaf-locked steady-state cache so Session can check whether
	// a stream is past its handshake padding epoch without blocking on pw.mu
	// while a TLS/H2 write is in flight. Lock order: pw.mu -> pw.readyMu.
	noStreamPadding  atomic.Bool
	hasPendingForget atomic.Bool
	readyMu          sync.RWMutex
	readySet         map[uint32]struct{}
	pendingForget    []uint32
}

// NewWriter creates a new padding-aware writer for the client role.
//
// Kept for compatibility; use NewWriterForRole to select the direction.
func NewWriter(w io.Writer, scheme *Scheme) *Writer {
	return NewWriterForRole(w, scheme, RoleClient)
}

// NewWriterForRole creates a padding-aware writer for the given direction.
// role selects the directional c<idx> / s<idx> padding rules.
func NewWriterForRole(w io.Writer, scheme *Scheme, role Role) *Writer {
	pw := &Writer{
		w:        w,
		scheme:   scheme,
		role:     role,
		fw:       protocol.NewFrameWriter(w),
		streams:  make(map[uint32]*streamState),
		readySet: make(map[uint32]struct{}),
	}
	pw.noStreamPadding.Store(scheme == nil || scheme.Stop <= 0)
	return pw
}

// UpdateScheme replaces the current padding scheme. It takes effect from the
// next write on this writer; per-stream epochs are preserved across updates.
func (pw *Writer) UpdateScheme(scheme *Scheme) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.drainPendingForgetsLocked()
	pw.scheme = scheme
	pw.noStreamPadding.Store(scheme == nil || scheme.Stop <= 0)
	for _, st := range pw.streams {
		st.ready = false
	}
	pw.readyMu.Lock()
	clear(pw.readySet)
	pw.readyMu.Unlock()
}

// ForgetStream drops the padding epoch of a stream. Sessions call it when the
// stream closes so closed streams do not occupy the bounded epoch table and so
// a future stream reusing the ID starts with a fresh epoch.
//
// If a network write currently holds pw.mu, ForgetStream defers deletion of
// pw.streams[id] instead of blocking the caller (e.g. Session.RunEventLoop),
// while immediately clearing readySet so the ID cannot coalesce.
func (pw *Writer) ForgetStream(id uint32) {
	pw.readyMu.Lock()
	delete(pw.readySet, id)
	pw.readyMu.Unlock()

	if pw.mu.TryLock() {
		delete(pw.streams, id)
		pw.drainPendingForgetsLocked()
		pw.mu.Unlock()
		return
	}

	pw.readyMu.Lock()
	pw.pendingForget = append(pw.pendingForget, id)
	pw.hasPendingForget.Store(true)
	pw.readyMu.Unlock()
}

func (pw *Writer) drainPendingForgetsLocked() {
	if !pw.hasPendingForget.Load() {
		return
	}
	pw.readyMu.Lock()
	ids := pw.pendingForget
	pw.pendingForget = nil
	pw.hasPendingForget.Store(false)
	pw.readyMu.Unlock()
	for _, id := range ids {
		delete(pw.streams, id)
	}
}

// CanCoalescePSH reports whether streamID is permanently past its handshake
// padding epoch under the current scheme and may therefore be safely coalesced
// into a multi-stream raw CmdPSH write batch under contention.
// It never acquires pw.mu, so callers do not block on in-flight conn writes.
func (pw *Writer) CanCoalescePSH(streamID uint32) bool {
	if streamID == 0 {
		return false
	}
	if pw.noStreamPadding.Load() {
		return true
	}
	pw.readyMu.RLock()
	_, ok := pw.readySet[streamID]
	pw.readyMu.RUnlock()
	return ok
}

func (pw *Writer) markReadyIfPastEpochLocked(streamID uint32, state *streamState) {
	if state == nil || state.ready || pw.scheme == nil {
		return
	}
	if state.idx >= pw.scheme.Stop || (pw.scheme.StreamBytes > 0 && state.bytes >= int64(pw.scheme.StreamBytes)) {
		state.ready = true
		pw.readyMu.Lock()
		pw.readySet[streamID] = struct{}{}
		pw.readyMu.Unlock()
	}
}

// WriteFramesWithPadding writes frames applying the PaddingScheme rules.
//
// The bucket is decided per call:
//   - every frame is CmdPSH with the same non-zero StreamID: that stream's
//     epoch decides whether/how to pad this write;
//   - anything else (settings, SYN/SYNACK/FIN, heartbeats, padding updates,
//     keepalive waste, mixed batches): the control bucket, which always uses
//     the scheme's ctl rule for the whole session lifetime.
//
// On the unpadded fast path frames are encoded into a buffer reused by this
// Writer (it is serialized by its own mutex), so that path does not allocate.
func (pw *Writer) WriteFramesWithPadding(frames []*protocol.Frame) error {
	if len(frames) == 0 {
		// Nothing to serialize and nothing to pad.
		return nil
	}
	for _, f := range frames {
		if f == nil {
			return protocol.ErrInvalidFrame
		}
		if len(f.Data) > protocol.MaxFrameDataLen {
			return protocol.ErrFrameTooLarge
		}
	}

	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.drainPendingForgetsLocked()

	if pw.scheme == nil {
		return pw.writeFramesRawLocked(frames)
	}

	if id, ok := dataStreamBucket(frames); ok {
		return pw.writeStreamLocked(frames, id)
	}
	return pw.writeControlLocked(frames)
}

// WritePSH writes raw stream payload bytes directly as CmdPSH frames without
// allocating intermediate Frame objects on the fast unpadded path.
func (pw *Writer) WritePSH(streamID uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if streamID == 0 {
		var frames [1]*protocol.Frame
		var f protocol.Frame
		for off := 0; off < len(data); {
			n := len(data) - off
			if n > protocol.MaxFrameDataLen {
				n = protocol.MaxFrameDataLen
			}
			f = protocol.Frame{Command: protocol.CmdPSH, StreamID: streamID, Data: data[off : off+n]}
			frames[0] = &f
			if err := pw.WriteFramesWithPadding(frames[:]); err != nil {
				return err
			}
			off += n
		}
		return nil
	}

	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.drainPendingForgetsLocked()

	if pw.scheme == nil {
		return pw.writePSHRawLocked(streamID, data)
	}

	state := pw.streams[streamID]
	if state == nil {
		pw.nextSeq++
		state = &streamState{insertedAt: time.Now(), seq: pw.nextSeq}
		pw.streams[streamID] = state
		pw.pruneStreamsLocked()
	}

	idx := state.idx
	state.idx++

	var segments []Segment
	if pw.scheme.StreamBytes <= 0 || state.bytes < int64(pw.scheme.StreamBytes) {
		segments = pw.scheme.GetSegmentsFor(pw.role, idx)
	}
	if segments == nil {
		if err := pw.writePSHRawLocked(streamID, data); err != nil {
			return err
		}
		state.bytes += int64(pshWireLen(len(data)))
		pw.markReadyIfPastEpochLocked(streamID, state)
		return nil
	}

	bufp, serialized := serializePSHPooled(streamID, data)
	written, err := pw.applySegments(serialized, segments, true)
	putPaddingBuf(bufp, serialized)
	state.bytes += int64(written)
	pw.markReadyIfPastEpochLocked(streamID, state)
	return err
}

// WritePSHPreframed writes an already header-prefixed single CmdPSH frame in-place
// with zero payload copy once the stream's handshake padding epoch has completed.
// If the stream is still within its handshake padding epoch, it applies the epoch
// rules directly under the same lock without mutating caller bytes past len(framed).
func (pw *Writer) WritePSHPreframed(streamID uint32, framed []byte, payloadLen int) (bool, error) {
	if streamID == 0 || payloadLen <= 0 || payloadLen > protocol.MaxFrameDataLen || len(framed) != protocol.FrameHeaderLen+payloadLen {
		return false, nil
	}

	// Ultra-fast steady-state path: stream is already permanently past its
	// handshake padding epoch under the current scheme.
	if pw.CanCoalescePSH(streamID) {
		pw.mu.Lock()
		_, err := pw.w.Write(framed)
		pw.mu.Unlock()
		return true, err
	}

	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.drainPendingForgetsLocked()

	if pw.scheme == nil {
		_, err := pw.w.Write(framed)
		return true, err
	}

	state := pw.streams[streamID]
	if state == nil {
		pw.nextSeq++
		state = &streamState{insertedAt: time.Now(), seq: pw.nextSeq}
		pw.streams[streamID] = state
		pw.pruneStreamsLocked()
	}

	idx := state.idx
	state.idx++

	var segments []Segment
	if pw.scheme.StreamBytes <= 0 || state.bytes < int64(pw.scheme.StreamBytes) {
		segments = pw.scheme.GetSegmentsFor(pw.role, idx)
	}
	if segments == nil {
		_, err := pw.w.Write(framed)
		if err == nil {
			state.bytes += int64(len(framed))
			pw.markReadyIfPastEpochLocked(streamID, state)
		}
		return true, err
	}

	written, err := pw.applySegments(framed, segments, false)
	state.bytes += int64(written)
	pw.markReadyIfPastEpochLocked(streamID, state)
	return true, err
}

// WriteSYNWithTarget coalesces CmdSYN + the initial target-address CmdPSH into a
// single padded record using stream rule 0 (e.g. c0=~80-200), advancing the
// stream's epoch to idx=1 so the subsequent inner TLS ClientHello still hits c1.
func (pw *Writer) WriteSYNWithTarget(streamID uint32, addrData []byte) error {
	if streamID == 0 || len(addrData) == 0 || len(addrData) > protocol.MaxFrameDataLen {
		return protocol.ErrInvalidFrame
	}

	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.drainPendingForgetsLocked()

	total := protocol.FrameHeaderLen*2 + len(addrData)
	minCap := total
	if minCap < MaxPadTarget {
		minCap = MaxPadTarget
	}
	bufp, raw := getPaddingBuf(minCap)
	buf := raw[:total]
	buf[0] = byte(protocol.CmdSYN)
	binary.BigEndian.PutUint32(buf[1:5], streamID)
	buf[5] = 0
	buf[6] = 0

	off := protocol.FrameHeaderLen
	buf[off] = byte(protocol.CmdPSH)
	binary.BigEndian.PutUint32(buf[off+1:off+5], streamID)
	binary.BigEndian.PutUint16(buf[off+5:off+7], uint16(len(addrData)))
	copy(buf[off+protocol.FrameHeaderLen:], addrData)
	defer putPaddingBuf(bufp, buf)

	if pw.scheme == nil {
		_, err := pw.w.Write(buf)
		return err
	}

	state := pw.streams[streamID]
	if state == nil {
		pw.nextSeq++
		state = &streamState{insertedAt: time.Now(), seq: pw.nextSeq}
		pw.streams[streamID] = state
		pw.pruneStreamsLocked()
	}
	idx := state.idx
	state.idx++

	var segments []Segment
	if pw.scheme.StreamBytes <= 0 || state.bytes < int64(pw.scheme.StreamBytes) {
		segments = pw.scheme.GetSegmentsFor(pw.role, idx)
	}
	if segments == nil {
		_, err := pw.w.Write(buf)
		if err == nil {
			state.bytes += int64(len(buf))
			pw.markReadyIfPastEpochLocked(streamID, state)
		}
		return err
	}

	written, err := pw.applySegments(buf, segments, true)
	state.bytes += int64(written)
	pw.markReadyIfPastEpochLocked(streamID, state)
	return err
}

// writeStreamLocked applies the per-stream epoch to a data write.
func (pw *Writer) writeStreamLocked(frames []*protocol.Frame, id uint32) error {
	state := pw.streams[id]
	if state == nil {
		pw.nextSeq++
		state = &streamState{insertedAt: time.Now(), seq: pw.nextSeq}
		pw.streams[id] = state
		pw.pruneStreamsLocked()
	}

	idx := state.idx
	state.idx++

	var segments []Segment
	if pw.scheme.StreamBytes <= 0 || state.bytes < int64(pw.scheme.StreamBytes) {
		segments = pw.scheme.GetSegmentsFor(pw.role, idx)
	}
	if segments == nil {
		if err := pw.writeFramesRawLocked(frames); err != nil {
			return err
		}
		state.bytes += int64(wireLen(frames))
		pw.markReadyIfPastEpochLocked(id, state)
		return nil
	}

	bufp, serialized := serializeFramesPooled(frames)
	written, err := pw.applySegments(serialized, segments, true)
	putPaddingBuf(bufp, serialized)
	state.bytes += int64(written)
	pw.markReadyIfPastEpochLocked(id, state)
	return err
}

// writeControlLocked applies the ctl rule to a streamless write.
//
// Control padding is intentionally NOT limited by stop / stream_bytes: SYN,
// SYNACK, FIN, heartbeats and padding updates keep occurring for the whole
// life of the session, and leaving them unpadded after a short epoch would
// leak tiny, highly regular record sizes.
func (pw *Writer) writeControlLocked(frames []*protocol.Frame) error {
	pw.ctlIdx++
	segments := pw.scheme.ControlRules
	if segments == nil {
		if err := pw.writeFramesRawLocked(frames); err != nil {
			return err
		}
		pw.ctlBytes += int64(wireLen(frames))
		return nil
	}

	bufp, serialized := serializeFramesPooled(frames)
	written, err := pw.applySegments(serialized, segments, true)
	putPaddingBuf(bufp, serialized)
	pw.ctlBytes += int64(written)
	return err
}

// applySegments applies a padding rule to the serialized write and returns the
// number of bytes written to the underlying writer. When inPlace is true,
// userData is owned by this writer and writePadTo may append waste in-place.
func (pw *Writer) applySegments(userData []byte, segments []Segment, inPlace bool) (int, error) {
	written := 0
	offset := 0
	remaining := len(userData)

	for _, seg := range segments {
		if seg.IsPadTo {
			// Pad-to consumes the rest of the payload and ends the rule.
			n, err := pw.writePadTo(userData[offset:], seg, inPlace && offset == 0)
			written += n
			return written, err
		}

		if seg.IsCheck {
			// 'c' check: if user data is exhausted, stop sending
			if remaining <= 0 {
				return written, nil
			}
			continue
		}

		targetLen := RandomInRange(seg.MinSize, seg.MaxSize)
		if targetLen < 0 {
			targetLen = 0
		}

		if remaining >= targetLen {
			// Enough user data — send a chunk of user data
			n, err := pw.w.Write(userData[offset : offset+targetLen])
			written += n
			if err != nil {
				return written, err
			}
			offset += targetLen
			remaining -= targetLen
		} else if remaining > 0 {
			// Not enough user data — send remaining user data + waste padding
			// Subtract FrameHeaderLen because the waste frame wire format adds its own header
			wasteSize := clampWasteSize(targetLen - remaining - protocol.FrameHeaderLen)
			totalLen := remaining + protocol.FrameHeaderLen + wasteSize
			bufp, raw := getPaddingBuf(totalLen)
			out := raw[:totalLen]
			copy(out, userData[offset:offset+remaining])
			waste := out[remaining:]
			waste[0] = byte(protocol.CmdWaste)
			waste[1] = 0
			waste[2] = 0
			waste[3] = 0
			waste[4] = 0
			binary.BigEndian.PutUint16(waste[5:7], uint16(wasteSize))
			clear(waste[protocol.FrameHeaderLen:])

			n, err := pw.w.Write(out)
			putPaddingBuf(bufp, out)
			written += n
			if err != nil {
				return written, err
			}
			offset += remaining
			remaining = 0
		} else {
			// No user data left — send pure waste padding
			wasteSize := clampWasteSize(targetLen - protocol.FrameHeaderLen)
			totalLen := protocol.FrameHeaderLen + wasteSize
			bufp, raw := getPaddingBuf(totalLen)
			out := raw[:totalLen]
			out[0] = byte(protocol.CmdWaste)
			out[1] = 0
			out[2] = 0
			out[3] = 0
			out[4] = 0
			binary.BigEndian.PutUint16(out[5:7], uint16(wasteSize))
			clear(out[protocol.FrameHeaderLen:])

			n, err := pw.w.Write(out)
			putPaddingBuf(bufp, out)
			if err != nil {
				return written, err
			}
			written += n
		}
	}

	// If user data still has remaining bytes after all segments, send directly
	if remaining > 0 {
		n, err := pw.w.Write(userData[offset:])
		written += n
		return written, err
	}

	return written, nil
}

// writePadTo emits payload as a SINGLE write padded to a random target size in
// [MinSize, MaxSize] by appending one cmdWaste frame inside the same write.
//
// The target is clamped to [len(payload)+FrameHeaderLen, MaxPadTarget]:
//   - the lower bound guarantees room for the waste frame header, so padding is
//     possible as long as the payload itself is below the record cap;
//   - the upper bound is the TLS record plaintext limit.
//
// If the payload is already at least as large as the requested target, it is
// written unchanged (one record, no waste frame, never split). The same applies
// when the payload leaves no room for a waste header below MaxPadTarget.
func (pw *Writer) writePadTo(payload []byte, seg Segment, inPlace bool) (int, error) {
	target := RandomInRange(seg.MinSize, seg.MaxSize)
	if len(payload) >= target {
		// Already at least as large as the wanted record: nothing to add.
		n, err := pw.w.Write(payload)
		return n, err
	}

	minTarget := len(payload) + protocol.FrameHeaderLen
	if minTarget > MaxPadTarget {
		// Payload alone already reaches the TLS record limit: one record, no
		// room for an appended waste frame.
		n, err := pw.w.Write(payload)
		return n, err
	}
	if target < minTarget {
		target = minTarget
	}
	if target > MaxPadTarget {
		target = MaxPadTarget
	}
	wasteSize := target - minTarget

	// Fast path: append CmdWaste in-place inside our pooled serialization buffer
	// to avoid a second full payload copy on every ctl / c0..c5 / s0..s5 write.
	if inPlace && cap(payload) >= target {
		out := payload[:target]
		waste := out[len(payload):]
		waste[0] = byte(protocol.CmdWaste)
		waste[1] = 0
		waste[2] = 0
		waste[3] = 0
		waste[4] = 0
		binary.BigEndian.PutUint16(waste[5:7], uint16(wasteSize))
		clear(waste[protocol.FrameHeaderLen:])
		return pw.w.Write(out)
	}

	bufp, raw := getPaddingBuf(target)
	out := raw[:target]
	copy(out, payload)
	waste := out[len(payload):]
	waste[0] = byte(protocol.CmdWaste)
	waste[1] = 0
	waste[2] = 0
	waste[3] = 0
	waste[4] = 0
	binary.BigEndian.PutUint16(waste[5:7], uint16(wasteSize))
	clear(waste[protocol.FrameHeaderLen:])

	n, err := pw.w.Write(out)
	putPaddingBuf(bufp, out)
	return n, err
}

func pshWireLen(dataLen int) int {
	if dataLen <= 0 {
		return 0
	}
	chunks := (dataLen + protocol.MaxFrameDataLen - 1) / protocol.MaxFrameDataLen
	return chunks*protocol.FrameHeaderLen + dataLen
}

func serializeFramesPooled(frames []*protocol.Frame) (*[]byte, []byte) {
	size := wireLen(frames)
	minCap := size
	if minCap < MaxPadTarget {
		minCap = MaxPadTarget
	}
	bufp, buf := getPaddingBuf(minCap)
	var header [protocol.FrameHeaderLen]byte
	for _, f := range frames {
		header[0] = byte(f.Command)
		header[1] = byte(f.StreamID >> 24)
		header[2] = byte(f.StreamID >> 16)
		header[3] = byte(f.StreamID >> 8)
		header[4] = byte(f.StreamID)
		header[5] = byte(len(f.Data) >> 8)
		header[6] = byte(len(f.Data))
		buf = append(buf, header[:]...)
		buf = append(buf, f.Data...)
	}
	return bufp, buf
}

func serializePSHPooled(streamID uint32, data []byte) (*[]byte, []byte) {
	size := pshWireLen(len(data))
	minCap := size
	if minCap < MaxPadTarget {
		minCap = MaxPadTarget
	}
	bufp, buf := getPaddingBuf(minCap)
	var header [protocol.FrameHeaderLen]byte
	for off := 0; off < len(data); {
		n := len(data) - off
		if n > protocol.MaxFrameDataLen {
			n = protocol.MaxFrameDataLen
		}
		header[0] = byte(protocol.CmdPSH)
		header[1] = byte(streamID >> 24)
		header[2] = byte(streamID >> 16)
		header[3] = byte(streamID >> 8)
		header[4] = byte(streamID)
		header[5] = byte(n >> 8)
		header[6] = byte(n)
		buf = append(buf, header[:]...)
		buf = append(buf, data[off:off+n]...)
		off += n
	}
	return bufp, buf
}

func (pw *Writer) writePSHRawLocked(streamID uint32, data []byte) error {
	total := pshWireLen(len(data))
	bufp, buf := getPaddingBuf(total)
	var header [protocol.FrameHeaderLen]byte
	for off := 0; off < len(data); {
		n := len(data) - off
		if n > protocol.MaxFrameDataLen {
			n = protocol.MaxFrameDataLen
		}
		header[0] = byte(protocol.CmdPSH)
		header[1] = byte(streamID >> 24)
		header[2] = byte(streamID >> 16)
		header[3] = byte(streamID >> 8)
		header[4] = byte(streamID)
		header[5] = byte(n >> 8)
		header[6] = byte(n)
		buf = append(buf, header[:]...)
		buf = append(buf, data[off:off+n]...)
		off += n
	}
	_, err := pw.w.Write(buf)
	putPaddingBuf(bufp, buf)
	return err
}

// maxReusableRawBuf bounds the pooled encoding buffer size. Writes up to this
// size reuse shared pooled buffers without allocating; larger writes fall back
// to the FrameWriter encoder so huge bursts do not pin memory.
const maxReusableRawBuf = 264 * 1024

// writeFramesRawLocked encodes frames into a pooled buffer and writes them in
// one call without padding. pw.mu must be held.
func (pw *Writer) writeFramesRawLocked(frames []*protocol.Frame) error {
	total := wireLen(frames)
	if total > maxReusableRawBuf {
		return pw.fw.WriteFrames(frames)
	}
	bufp, buf := getPaddingBuf(total)
	var header [protocol.FrameHeaderLen]byte
	for _, f := range frames {
		header[0] = byte(f.Command)
		header[1] = byte(f.StreamID >> 24)
		header[2] = byte(f.StreamID >> 16)
		header[3] = byte(f.StreamID >> 8)
		header[4] = byte(f.StreamID)
		header[5] = byte(len(f.Data) >> 8)
		header[6] = byte(len(f.Data))
		buf = append(buf, header[:]...)
		buf = append(buf, f.Data...)
	}
	_, err := pw.w.Write(buf)
	putPaddingBuf(bufp, buf)
	return err
}

// prunesStreamsLocked removes the oldest stream state when the table is over
// the limit. pw.mu must be held.
func (pw *Writer) pruneStreamsLocked() {
	if len(pw.streams) <= maxTrackedStreams {
		return
	}
	var oldestID uint32
	var oldest *streamState
	for id, state := range pw.streams {
		if oldest == nil ||
			state.insertedAt.Before(oldest.insertedAt) ||
			(state.insertedAt.Equal(oldest.insertedAt) && state.seq < oldest.seq) {
			oldestID, oldest = id, state
		}
	}
	delete(pw.streams, oldestID)
	pw.readyMu.Lock()
	delete(pw.readySet, oldestID)
	pw.readyMu.Unlock()
}

// dataStreamBucket reports the stream a write belongs to when every frame is
// CmdPSH with the same non-zero StreamID.
func dataStreamBucket(frames []*protocol.Frame) (uint32, bool) {
	id := frames[0].StreamID
	if id == 0 {
		return 0, false
	}
	for _, f := range frames {
		if f.Command != protocol.CmdPSH || f.StreamID != id {
			return 0, false
		}
	}
	return id, true
}

// wireLen returns the TSUNAMI wire size of the given frames.
func wireLen(frames []*protocol.Frame) int {
	total := 0
	for _, f := range frames {
		total += protocol.FrameHeaderLen + len(f.Data)
	}
	return total
}

// clampWasteSize guards generated cmdWaste frames: a frame payload must fit in
// the 16-bit wire length field (protocol.MaxFrameDataLen). Scheme segment sizes
// are operator-controlled, so huge ranges are clamped instead of producing
// malformed frames.
func clampWasteSize(size int) int {
	if size < 0 {
		return 0
	}
	if size > protocol.MaxFrameDataLen {
		return protocol.MaxFrameDataLen
	}
	return size
}

// WriteRaw writes raw bytes directly without padding (used after padding stop).
// It bypasses epoch and budget bookkeeping.
func (pw *Writer) WriteRaw(data []byte) error {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	_, err := pw.w.Write(data)
	return err
}

// PacketIndex returns the number of control-bucket writes processed by this
// writer.
//
// Before per-stream epochs this used to be the per-session TLS write counter.
// There is no single session-wide data-write counter anymore, so the value now
// tracks the control bucket only (settings, SYN/SYNACK/FIN, heartbeats,
// padding updates, keepalive waste). Kept for callers that only need a
// monotonic progress indicator.
func (pw *Writer) PacketIndex() int {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.ctlIdx
}

// ControlBytes returns the total number of bytes (payload + padding) written
// for control-bucket writes. It is exposed for observability; control padding
// itself is not limited by any budget.
func (pw *Writer) ControlBytes() int64 {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.ctlBytes
}

// KeepaliveGenerator sends minimal waste packets during idle periods.
// Behaves like a TLS heartbeat: 30-60s intervals, 4-8 byte packets.
// Only active when all Streams in the Session are idle.
type KeepaliveGenerator struct {
	config   *KeepaliveConfig
	stopCh   chan struct{}
	once     sync.Once
	isActive atomic.Bool                   // tracks whether streams are active
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
