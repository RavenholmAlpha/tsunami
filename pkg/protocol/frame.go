package protocol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Frame represents a single session-layer frame.
//
// Wire format:
//
//	+----------+-------------------+-------------------+------+
//	| command  | streamId          | data length       | data |
//	+----------+-------------------+-------------------+------+
//	| uint8    | uint32 Big-Endian | uint16 Big-Endian | var  |
//	+----------+-------------------+-------------------+------+
//	       Total header overhead: 7 Bytes
type Frame struct {
	Command  Command
	StreamID uint32
	Data     []byte
}

var (
	ErrFrameTooLarge       = errors.New("tsunami: frame data exceeds maximum length")
	ErrInvalidFrame        = errors.New("tsunami: invalid frame")
	ErrStreamClosed        = errors.New("tsunami: stream closed")
	ErrSessionClosed       = errors.New("tsunami: session closed")
	ErrAuthFailed          = errors.New("tsunami: authentication failed")
	ErrAuthPaddingTooLarge = errors.New("tsunami: authentication padding exceeds maximum length")
	ErrVersionMismatch     = errors.New("tsunami: version mismatch")
	ErrStreamRejected      = errors.New("tsunami: stream rejected")
	ErrTooManyStreams      = errors.New("tsunami: too many concurrent streams")
	ErrStreamRecvOverflow  = errors.New("tsunami: stream receive buffer overflow")
)

// frameBufPool reuses frame encode buffers to reduce GC pressure under high throughput.
var frameBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, MaxFrameDataLen+FrameHeaderLen)
		return &buf
	},
}

// RelayBufPool provides MaxFrameDataLen-sized (65535 B) buffers for bidirectional
// relay copies so a full read maps to a single TSUNAMI PSH frame without an extra
// 1-byte tail frame.
var RelayBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, MaxFrameDataLen)
		return &buf
	},
}

// writeCoalesceBufPool provides buffers for WriteFrames coalescing.
var writeCoalesceBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 128*1024)
		return &buf
	},
}

const (
	recvPoolSmallCap  = 4 * 1024
	recvPoolMediumCap = 16 * 1024
	recvPoolLargeCap  = MaxFrameDataLen
	maxCoalescePool   = 512 * 1024
)

var (
	recvPoolSmall = sync.Pool{
		New: func() any {
			buf := make([]byte, recvPoolSmallCap)
			return &buf
		},
	}
	recvPoolMedium = sync.Pool{
		New: func() any {
			buf := make([]byte, recvPoolMediumCap)
			return &buf
		},
	}
	recvPoolLarge = sync.Pool{
		New: func() any {
			buf := make([]byte, recvPoolLargeCap)
			return &buf
		},
	}
)

func getRecvPayloadBuf(n int) (*[]byte, []byte) {
	var bufp *[]byte
	switch {
	case n <= recvPoolSmallCap:
		bufp = recvPoolSmall.Get().(*[]byte)
	case n <= recvPoolMediumCap:
		bufp = recvPoolMedium.Get().(*[]byte)
	default:
		bufp = recvPoolLarge.Get().(*[]byte)
	}
	return bufp, (*bufp)[:n]
}

func putRecvPayloadBuf(bufp *[]byte) {
	if bufp == nil {
		return
	}
	switch cap(*bufp) {
	case recvPoolSmallCap:
		*bufp = (*bufp)[:recvPoolSmallCap]
		recvPoolSmall.Put(bufp)
	case recvPoolMediumCap:
		*bufp = (*bufp)[:recvPoolMediumCap]
		recvPoolMedium.Put(bufp)
	case recvPoolLargeCap:
		*bufp = (*bufp)[:recvPoolLargeCap]
		recvPoolLarge.Put(bufp)
	}
}

// EncodeFrame writes a frame to the writer in TSUNAMI wire format.
// The data length MUST NOT exceed MaxFrameDataLen (65535).
func EncodeFrame(w io.Writer, f *Frame) error {
	if f == nil {
		return ErrInvalidFrame
	}
	dataLen := len(f.Data)
	if dataLen > MaxFrameDataLen {
		return ErrFrameTooLarge
	}

	if dataLen == 0 {
		// Header-only frame — use stack-allocated buffer, no pool needed
		var header [FrameHeaderLen]byte
		header[0] = byte(f.Command)
		binary.BigEndian.PutUint32(header[1:5], f.StreamID)
		binary.BigEndian.PutUint16(header[5:7], 0)
		_, err := w.Write(header[:])
		return err
	}

	// Data frame — use pooled buffer to avoid per-frame allocation
	poolBufp := frameBufPool.Get().(*[]byte)
	buf := (*poolBufp)[:FrameHeaderLen+dataLen]

	buf[0] = byte(f.Command)
	binary.BigEndian.PutUint32(buf[1:5], f.StreamID)
	binary.BigEndian.PutUint16(buf[5:7], uint16(dataLen))
	copy(buf[FrameHeaderLen:], f.Data)

	_, err := w.Write(buf)
	*poolBufp = (*poolBufp)[:MaxFrameDataLen+FrameHeaderLen]
	frameBufPool.Put(poolBufp)
	return err
}

// DecodeFrame reads a single frame from the reader.
func DecodeFrame(r io.Reader) (*Frame, error) {
	var header [FrameHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("tsunami: read frame header: %w", err)
	}

	f := &Frame{
		Command:  Command(header[0]),
		StreamID: binary.BigEndian.Uint32(header[1:5]),
	}
	dataLen := int(binary.BigEndian.Uint16(header[5:7]))

	if dataLen > 0 {
		f.Data = make([]byte, dataLen)
		if _, err := io.ReadFull(r, f.Data); err != nil {
			return nil, fmt.Errorf("tsunami: read frame data: %w", err)
		}
	}

	return f, nil
}

// NewFrame creates a new Frame with the given command, stream ID, and optional data.
func NewFrame(cmd Command, streamID uint32, data []byte) *Frame {
	return &Frame{
		Command:  cmd,
		StreamID: streamID,
		Data:     data,
	}
}

// NewWasteFrame creates a cmdWaste padding frame with the specified number of zero bytes.
func NewWasteFrame(size int) *Frame {
	if size < 0 {
		size = 0
	}
	return &Frame{
		Command:  CmdWaste,
		StreamID: 0,
		Data:     make([]byte, size),
	}
}

// FrameWriter provides buffered frame writing with optional padding support.
type FrameWriter struct {
	w io.Writer
}

// NewFrameWriter creates a new FrameWriter.
func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{w: w}
}

// WriteFrame encodes and writes a single frame.
func (fw *FrameWriter) WriteFrame(f *Frame) error {
	return EncodeFrame(fw.w, f)
}

// WriteFrames writes multiple frames in a single buffer to minimize TLS records.
func (fw *FrameWriter) WriteFrames(frames []*Frame) error {
	totalLen := 0
	for _, f := range frames {
		if f == nil {
			return ErrInvalidFrame
		}
		if len(f.Data) > MaxFrameDataLen {
			return ErrFrameTooLarge
		}
		totalLen += FrameHeaderLen + len(f.Data)
	}

	bufp := writeCoalesceBufPool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < totalLen {
		buf = make([]byte, 0, totalLen)
	}
	buf = buf[:0]

	var header [FrameHeaderLen]byte
	for _, f := range frames {
		header[0] = byte(f.Command)
		binary.BigEndian.PutUint32(header[1:5], f.StreamID)
		binary.BigEndian.PutUint16(header[5:7], uint16(len(f.Data)))
		buf = append(buf, header[:]...)
		if len(f.Data) > 0 {
			buf = append(buf, f.Data...)
		}
	}

	_, err := fw.w.Write(buf)
	if cap(buf) <= maxCoalescePool {
		*bufp = buf[:0]
		writeCoalesceBufPool.Put(bufp)
	}
	return err
}

// FrameReader provides frame reading from a connection.
type FrameReader struct {
	r       io.Reader
	waste   []byte
	scratch Frame
	ctrlBuf [64]byte
}

// NewFrameReader creates a new FrameReader.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r}
}

// NewBufferedFrameReader creates a FrameReader backed by a bufio.Reader of at
// least bufSize bytes so header+payload reads do not issue two syscalls per frame.
func NewBufferedFrameReader(r io.Reader, bufSize int) *FrameReader {
	if _, ok := r.(*bufio.Reader); !ok && bufSize > 0 {
		r = bufio.NewReaderSize(r, bufSize)
	}
	return &FrameReader{r: r}
}

// ReadFrame reads and decodes the next frame.
func (fr *FrameReader) ReadFrame() (*Frame, error) {
	return DecodeFrame(fr.r)
}

func (fr *FrameReader) readFramePooled() (*Frame, *[]byte, error) {
	var header [FrameHeaderLen]byte
	if _, err := io.ReadFull(fr.r, header[:]); err != nil {
		return nil, nil, fmt.Errorf("tsunami: read frame header: %w", err)
	}

	cmd := Command(header[0])
	streamID := binary.BigEndian.Uint32(header[1:5])
	dataLen := int(binary.BigEndian.Uint16(header[5:7]))

	if dataLen == 0 {
		fr.scratch = Frame{Command: cmd, StreamID: streamID}
		return &fr.scratch, nil, nil
	}

	if cmd == CmdWaste {
		if cap(fr.waste) < dataLen {
			fr.waste = make([]byte, dataLen)
		}
		if _, err := io.ReadFull(fr.r, fr.waste[:dataLen]); err != nil {
			return nil, nil, fmt.Errorf("tsunami: read frame data: %w", err)
		}
		fr.scratch = Frame{Command: CmdWaste}
		return &fr.scratch, nil, nil
	}

	if cmd == CmdPSH {
		bufp, payload := getRecvPayloadBuf(dataLen)
		if _, err := io.ReadFull(fr.r, payload); err != nil {
			putRecvPayloadBuf(bufp)
			return nil, nil, fmt.Errorf("tsunami: read frame data: %w", err)
		}
		fr.scratch = Frame{Command: cmd, StreamID: streamID, Data: payload}
		return &fr.scratch, bufp, nil
	}

	if dataLen <= len(fr.ctrlBuf) {
		data := fr.ctrlBuf[:dataLen]
		if _, err := io.ReadFull(fr.r, data); err != nil {
			return nil, nil, fmt.Errorf("tsunami: read frame data: %w", err)
		}
		fr.scratch = Frame{Command: cmd, StreamID: streamID, Data: data}
		return &fr.scratch, nil, nil
	}

	data := make([]byte, dataLen)
	if _, err := io.ReadFull(fr.r, data); err != nil {
		return nil, nil, fmt.Errorf("tsunami: read frame data: %w", err)
	}
	fr.scratch = Frame{Command: cmd, StreamID: streamID, Data: data}
	return &fr.scratch, nil, nil
}
