package fronting

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type bufferedFlushWriter struct {
	header http.Header
	raw    *bytes.Buffer
	bw     *bufio.Writer
}

func newBufferedFlushWriter(size int) *bufferedFlushWriter {
	raw := &bytes.Buffer{}
	return &bufferedFlushWriter{
		header: make(http.Header),
		raw:    raw,
		bw:     bufio.NewWriterSize(raw, size),
	}
}

func (w *bufferedFlushWriter) Header() http.Header         { return w.header }
func (w *bufferedFlushWriter) WriteHeader(status int)    {}
func (w *bufferedFlushWriter) Write(p []byte) (int, error) { return w.bw.Write(p) }
func (w *bufferedFlushWriter) Flush()                    { _ = w.bw.Flush() }

func TestHTTPServerConnFlushesExact4096Bytes(t *testing.T) {
	bw := newBufferedFlushWriter(4096)
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/assets/update", http.NoBody)
	conn := NewHTTPServerConn(bw, req)

	// Exactly 4096 bytes matches http2's handlerChunkWriteSize (bufio.Writer size 4096):
	// bufio.Writer buffers exact-size writes until Flush() is called.
	payload := bytes.Repeat([]byte("Z"), 4096)
	n, err := conn.Write(payload)
	if err != nil {
		t.Fatalf("Write 4096 bytes: %v", err)
	}
	if n != 4096 {
		t.Fatalf("Write n = %d, want 4096", n)
	}
	if got := bw.raw.Len(); got != 4096 {
		t.Fatalf("flushed bytes = %d, want 4096 immediately (4096-byte stall bug)", got)
	}
}

func TestHTTPClientConnReadDeadlineBeforeResponse(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	respCh := make(chan HTTPResponseResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var closedCalled atomic.Bool
	conn := NewPendingHTTPClientConnWithCleanup(respCh, pw, "example.com:443", cancel, func() {
		closedCalled.Store(true)
	})
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 16)
	_, err := conn.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read error = %v, want os.ErrDeadlineExceeded", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Read error does not implement Timeout()==true: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("request context was not canceled on read timeout")
	}
	if !closedCalled.Load() {
		t.Fatal("onClose cleanup hook was not called")
	}
}

func TestHTTPClientConnWriteDeadlineUnblocksPipe(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	respCh := make(chan HTTPResponseResult, 1)
	conn := NewPendingHTTPClientConnWithCleanup(respCh, pw, "example.com:443", nil, nil)
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	_, err := conn.Write([]byte("blocked-pipe-write"))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write error = %v, want os.ErrDeadlineExceeded", err)
	}
}

type trackingReadCloser struct {
	closed atomic.Bool
}

func (r *trackingReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (r *trackingReadCloser) Close() error {
	r.closed.Store(true)
	return nil
}

func TestHTTPClientConnCloseDrainsPendingResponseBody(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	respCh := make(chan HTTPResponseResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var onCloseCalled atomic.Bool
	conn := NewPendingHTTPClientConnWithCleanup(respCh, pw, "example.com:443", cancel, func() {
		onCloseCalled.Store(true)
	})

	body := &trackingReadCloser{}
	respCh <- HTTPResponseResult{Response: &http.Response{StatusCode: http.StatusOK, Body: body}}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("request context not canceled on Close")
	}
	if !onCloseCalled.Load() {
		t.Fatal("onClose not called")
	}
	if !body.closed.Load() {
		t.Fatal("pending response body was leaked on Close")
	}
}
