package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	ErrFrameTooLarge  = errors.New("tsunami: frame data exceeds maximum length")
	ErrInvalidFrame   = errors.New("tsunami: invalid frame")
	ErrStreamClosed   = errors.New("tsunami: stream closed")
	ErrSessionClosed  = errors.New("tsunami: session closed")
	ErrAuthFailed     = errors.New("tsunami: authentication failed")
	ErrVersionMismatch = errors.New("tsunami: version mismatch")
)

// EncodeFrame writes a frame to the writer in TSUNAMI wire format.
// The data length MUST NOT exceed MaxFrameDataLen (65535).
func EncodeFrame(w io.Writer, f *Frame) error {
	dataLen := len(f.Data)
	if dataLen > MaxFrameDataLen {
		return ErrFrameTooLarge
	}

	// Build the 7-byte header
	var header [FrameHeaderLen]byte
	header[0] = byte(f.Command)
	binary.BigEndian.PutUint32(header[1:5], f.StreamID)
	binary.BigEndian.PutUint16(header[5:7], uint16(dataLen))

	// Write header + data in one call if possible to reduce syscalls
	if dataLen > 0 {
		buf := make([]byte, FrameHeaderLen+dataLen)
		copy(buf[:FrameHeaderLen], header[:])
		copy(buf[FrameHeaderLen:], f.Data)
		_, err := w.Write(buf)
		return err
	}

	_, err := w.Write(header[:])
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
	dataLen := binary.BigEndian.Uint16(header[5:7])

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
		totalLen += FrameHeaderLen + len(f.Data)
	}

	buf := make([]byte, 0, totalLen)
	for _, f := range frames {
		var header [FrameHeaderLen]byte
		header[0] = byte(f.Command)
		binary.BigEndian.PutUint32(header[1:5], f.StreamID)
		binary.BigEndian.PutUint16(header[5:7], uint16(len(f.Data)))
		buf = append(buf, header[:]...)
		if len(f.Data) > 0 {
			buf = append(buf, f.Data...)
		}
	}

	_, err := fw.w.Write(buf)
	return err
}

// FrameReader provides frame reading from a connection.
type FrameReader struct {
	r io.Reader
}

// NewFrameReader creates a new FrameReader.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r}
}

// ReadFrame reads and decodes the next frame.
func (fr *FrameReader) ReadFrame() (*Frame, error) {
	return DecodeFrame(fr.r)
}
