package fronting

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPServerConn adapts an HTTP request/response pair into a net.Conn with real
// ResponseController deadlines and immediate flushing (avoiding bufio 4096-byte stalls).
type HTTPServerConn struct {
	body    io.ReadCloser
	writer  http.ResponseWriter
	flusher http.Flusher
	rc      *http.ResponseController

	remote net.Addr
	local  net.Addr

	writeMu sync.Mutex
	writing atomic.Bool
	closed  atomic.Bool
}

// NewHTTPServerConn returns a net.Conn backed by r.Body and w.
func NewHTTPServerConn(w http.ResponseWriter, r *http.Request) *HTTPServerConn {
	flusher, _ := w.(http.Flusher)
	return &HTTPServerConn{
		body:    r.Body,
		writer:  w,
		flusher: flusher,
		rc:      http.NewResponseController(w),
		remote:  addrString(r.RemoteAddr),
		local:   addrString(r.Host),
	}
}

func (c *HTTPServerConn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.body.Read(p)
}

func (c *HTTPServerConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() || c.writer == nil {
		return 0, net.ErrClosed
	}

	c.writing.Store(true)
	n, err := c.writer.Write(p)
	if n > 0 {
		if c.rc != nil {
			if fErr := c.rc.Flush(); err == nil && fErr != nil && !errors.Is(fErr, http.ErrNotSupported) {
				err = fErr
			}
		} else if c.flusher != nil {
			c.flusher.Flush()
		}
	}
	c.writing.Store(false)
	return n, err
}

func (c *HTTPServerConn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Close request body first to unblock any active Read cleanly.
	err := c.body.Close()
	// Only cancel an actively blocked Write via SetWriteDeadline(past), because
	// http2's SetWriteDeadline(past) resets the stream with INTERNAL_ERROR
	// instead of letting ServeHTTP finish with a clean END_STREAM.
	if c.writing.Load() && c.rc != nil {
		_ = c.rc.SetWriteDeadline(time.Unix(1, 0))
	}
	// Wait for any in-flight ResponseWriter.Write/Flush to finish and clear writer refs.
	c.writeMu.Lock()
	c.writer = nil
	c.flusher = nil
	c.writeMu.Unlock()
	return err
}

func (c *HTTPServerConn) LocalAddr() net.Addr  { return c.local }
func (c *HTTPServerConn) RemoteAddr() net.Addr { return c.remote }

func (c *HTTPServerConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *HTTPServerConn) SetReadDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if c.rc == nil {
		return nil
	}
	err := c.rc.SetReadDeadline(t)
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (c *HTTPServerConn) SetWriteDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if c.rc == nil {
		return nil
	}
	err := c.rc.SetWriteDeadline(t)
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// PipeWriteCloser represents a request-body stream writer supporting error propagation.
type PipeWriteCloser interface {
	io.WriteCloser
	CloseWithError(err error) error
}

// HTTPClientConn adapts an HTTP response body and request pipe into net.Conn
// with real timer-driven read/write deadlines and leak-free cleanup.
type HTTPClientConn struct {
	reader    io.ReadCloser
	writer    PipeWriteCloser
	respCh    <-chan HTTPResponseResult
	reqCancel context.CancelFunc
	onClose   func()

	remote net.Addr
	local  net.Addr

	initOnce sync.Once
	initErr  error

	mu                  sync.Mutex
	closed              bool
	closedCh            chan struct{}
	abortErr            error
	readTimer           *time.Timer
	readDeadlineAt      time.Time
	readDeadlineActive  bool
	writeTimer          *time.Timer
	writeTimerArmedAt   time.Time
	writeDeadlineAt     time.Time
	writeDeadlineActive bool
	readExpired         atomic.Bool
	writeExpired        atomic.Bool
	closeOnce           sync.Once
	closeErr            error
}

// HTTPResponseResult carries a pending HTTP tunnel response.
type HTTPResponseResult struct {
	Response *http.Response
	Err      error
}

// NewHTTPClientConn returns a net.Conn backed by resp.Body and the request body pipe.
func NewHTTPClientConn(resp *http.Response, writer PipeWriteCloser) *HTTPClientConn {
	remote := ""
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		remote = resp.Request.URL.Host
	}
	return &HTTPClientConn{
		reader:   resp.Body,
		writer:   writer,
		remote:   addrString(remote),
		local:    addrString(""),
		closedCh: make(chan struct{}),
	}
}

// NewPendingHTTPClientConn returns a net.Conn that can write request body data
// before the HTTP response headers have arrived. Reads wait for the response.
func NewPendingHTTPClientConn(respCh <-chan HTTPResponseResult, writer PipeWriteCloser, remote string) *HTTPClientConn {
	return NewPendingHTTPClientConnWithCleanup(respCh, writer, remote, nil, nil)
}

// NewPendingHTTPClientConnWithCleanup creates an HTTPClientConn with request
// cancellation and transport cleanup hooks invoked on Close or deadline expiry.
func NewPendingHTTPClientConnWithCleanup(respCh <-chan HTTPResponseResult, writer PipeWriteCloser, remote string, cancel context.CancelFunc, onClose func()) *HTTPClientConn {
	return &HTTPClientConn{
		respCh:    respCh,
		writer:    writer,
		reqCancel: cancel,
		onClose:   onClose,
		remote:    addrString(remote),
		local:     addrString(""),
		closedCh:  make(chan struct{}),
	}
}

// IsClosed reports whether the connection has been closed or aborted.
func (c *HTTPClientConn) IsClosed() bool {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	return closed
}

func (c *HTTPClientConn) Read(p []byte) (int, error) {
	if err := c.ensureReader(); err != nil {
		return 0, err
	}
	n, err := c.reader.Read(p)
	if err != nil && c.readExpired.Load() {
		return n, os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *HTTPClientConn) Write(p []byte) (int, error) {
	if c.writer == nil {
		return 0, net.ErrClosed
	}
	if c.writeExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.writer.Write(p)
	if err != nil && c.writeExpired.Load() {
		return n, os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *HTTPClientConn) Close() error {
	return c.abort(net.ErrClosed)
}

func (c *HTTPClientConn) abort(err error) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		if err == nil {
			err = net.ErrClosed
		}
		c.abortErr = err
		c.readDeadlineActive = false
		c.writeDeadlineActive = false
		if c.readTimer != nil {
			c.readTimer.Stop()
		}
		if c.writeTimer != nil {
			c.writeTimer.Stop()
		}
		reader := c.reader
		cancel := c.reqCancel
		onClose := c.onClose
		c.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		var errs []error
		if c.writer != nil {
			_ = c.writer.CloseWithError(err)
		}
		if reader != nil {
			if rErr := reader.Close(); rErr != nil {
				errs = append(errs, rErr)
			}
		}
		// Drain any unconsumed pending response so its body cannot leak.
		if c.respCh != nil {
			select {
			case res := <-c.respCh:
				if res.Response != nil && res.Response.Body != nil {
					_ = res.Response.Body.Close()
				}
			default:
			}
		}
		if onClose != nil {
			onClose()
		}
		c.closeErr = errors.Join(errs...)
		close(c.closedCh)
	})
	return c.closeErr
}

func (c *HTTPClientConn) LocalAddr() net.Addr  { return c.local }
func (c *HTTPClientConn) RemoteAddr() net.Addr { return c.remote }

func (c *HTTPClientConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *HTTPClientConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		err := c.abortErr
		if err == nil {
			err = net.ErrClosed
		}
		c.mu.Unlock()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		}
		return net.ErrClosed
	}
	if t.IsZero() {
		c.readDeadlineActive = false
		c.readExpired.Store(false)
		if c.readTimer != nil {
			c.readTimer.Stop()
		}
		c.mu.Unlock()
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		c.readDeadlineActive = false
		c.readExpired.Store(true)
		if c.readTimer != nil {
			c.readTimer.Stop()
		}
		c.mu.Unlock()
		_ = c.abort(os.ErrDeadlineExceeded)
		return nil
	}
	c.readExpired.Store(false)
	c.readDeadlineAt = t
	c.readDeadlineActive = true
	if c.readTimer == nil {
		c.readTimer = time.AfterFunc(d, c.onReadDeadline)
	} else {
		c.readTimer.Reset(d)
	}
	c.mu.Unlock()
	return nil
}

func (c *HTTPClientConn) onReadDeadline() {
	c.mu.Lock()
	if c.closed || !c.readDeadlineActive || time.Now().Before(c.readDeadlineAt) {
		c.mu.Unlock()
		return
	}
	c.readDeadlineActive = false
	c.readExpired.Store(true)
	c.mu.Unlock()
	_ = c.abort(os.ErrDeadlineExceeded)
}

func (c *HTTPClientConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		err := c.abortErr
		if err == nil {
			err = net.ErrClosed
		}
		c.mu.Unlock()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		}
		return net.ErrClosed
	}
	if t.IsZero() {
		c.writeDeadlineActive = false
		c.writeExpired.Store(false)
		c.mu.Unlock()
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		c.writeDeadlineActive = false
		c.writeTimerArmedAt = time.Time{}
		c.writeExpired.Store(true)
		if c.writeTimer != nil {
			c.writeTimer.Stop()
		}
		c.mu.Unlock()
		_ = c.abort(os.ErrDeadlineExceeded)
		return nil
	}
	c.writeExpired.Store(false)
	c.writeDeadlineAt = t
	c.writeDeadlineActive = true
	if c.writeTimer == nil {
		c.writeTimerArmedAt = t
		c.writeTimer = time.AfterFunc(d, c.onWriteDeadline)
	} else if c.writeTimerArmedAt.IsZero() || t.Before(c.writeTimerArmedAt) {
		c.writeTimerArmedAt = t
		c.writeTimer.Reset(d)
	}
	c.mu.Unlock()
	return nil
}

func (c *HTTPClientConn) onWriteDeadline() {
	c.mu.Lock()
	c.writeTimerArmedAt = time.Time{}
	if c.closed || !c.writeDeadlineActive {
		c.mu.Unlock()
		return
	}
	if remaining := time.Until(c.writeDeadlineAt); remaining > 0 {
		c.writeTimerArmedAt = c.writeDeadlineAt
		c.writeTimer.Reset(remaining)
		c.mu.Unlock()
		return
	}
	c.writeDeadlineActive = false
	c.writeExpired.Store(true)
	c.mu.Unlock()
	_ = c.abort(os.ErrDeadlineExceeded)
}

func (c *HTTPClientConn) ensureReader() error {
	c.initOnce.Do(func() {
		c.mu.Lock()
		hasReader := c.reader != nil
		c.mu.Unlock()
		if hasReader {
			return
		}
		if c.respCh == nil {
			c.initErr = net.ErrClosed
			return
		}
		select {
		case result := <-c.respCh:
			if result.Err != nil {
				c.initErr = result.Err
				return
			}
			if result.Response == nil || result.Response.Body == nil {
				c.initErr = io.ErrUnexpectedEOF
				return
			}
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				_ = result.Response.Body.Close()
				if c.readExpired.Load() {
					c.initErr = os.ErrDeadlineExceeded
				} else {
					c.initErr = net.ErrClosed
				}
				return
			}
			c.reader = result.Response.Body
			c.mu.Unlock()
		case <-c.closedCh:
			if c.readExpired.Load() {
				c.initErr = os.ErrDeadlineExceeded
			} else {
				c.initErr = net.ErrClosed
			}
		}
	})
	return c.initErr
}

type simpleAddr string

func (a simpleAddr) Network() string { return "fronting" }
func (a simpleAddr) String() string  { return string(a) }

func addrString(s string) net.Addr {
	return simpleAddr(s)
}

type bufferedPipe struct {
	mu           sync.Mutex
	buf          []byte
	head         int
	size         int
	maxBytes     int
	spaceWaiters int
	rClosed      bool
	rErr         error
	wClosed      bool
	wErr         error
	readNotify   chan struct{}
	spaceNotify  chan struct{}
	wClosedCh    chan struct{}
	rClosedCh    chan struct{}
	wCloseOnce   sync.Once
	rCloseOnce   sync.Once
}

// BufferedPipeReader reads from an in-memory bounded ring pipe.
type BufferedPipeReader struct {
	p *bufferedPipe
}

// BufferedPipeWriter writes to an in-memory bounded ring pipe without per-write synchronous handoff.
type BufferedPipeWriter struct {
	p *bufferedPipe
}

// NewBufferedPipe returns an asynchronous bounded in-memory pipe up to maxBytes.
func NewBufferedPipe(maxBytes int) (*BufferedPipeReader, *BufferedPipeWriter) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	p := &bufferedPipe{
		maxBytes:    maxBytes,
		readNotify:  make(chan struct{}, 1),
		spaceNotify: make(chan struct{}, 1),
		wClosedCh:   make(chan struct{}),
		rClosedCh:   make(chan struct{}),
	}
	return &BufferedPipeReader{p: p}, &BufferedPipeWriter{p: p}
}

func (r *BufferedPipeReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	p := r.p
	for {
		p.mu.Lock()
		if p.rClosed {
			p.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		if p.size > 0 {
			wasFull := p.size >= p.maxBytes
			n := len(dst)
			if n > p.size {
				n = p.size
			}
			first := len(p.buf) - p.head
			if first > n {
				first = n
			}
			copy(dst, p.buf[p.head:p.head+first])
			if first < n {
				copy(dst[first:], p.buf[:n-first])
			}
			p.head = (p.head + n) % len(p.buf)
			p.size -= n
			if p.size == 0 {
				p.head = 0
			}
			notifySpace := wasFull || p.spaceWaiters > 0
			p.mu.Unlock()

			if notifySpace {
				select {
				case p.spaceNotify <- struct{}{}:
				default:
				}
			}
			return n, nil
		}
		if p.wClosed {
			err := p.wErr
			if err == nil {
				err = io.EOF
			}
			p.mu.Unlock()
			return 0, err
		}
		p.mu.Unlock()

		select {
		case <-p.readNotify:
		case <-p.wClosedCh:
		case <-p.rClosedCh:
		}
	}
}

func (r *BufferedPipeReader) Close() error {
	return r.CloseWithError(nil)
}

func (r *BufferedPipeReader) CloseWithError(err error) error {
	p := r.p
	p.mu.Lock()
	if !p.rClosed {
		p.rClosed = true
		if err == nil {
			err = io.ErrClosedPipe
		}
		p.rErr = err
		p.buf = nil
		p.head = 0
		p.size = 0
	}
	p.mu.Unlock()
	p.rCloseOnce.Do(func() {
		close(p.rClosedCh)
	})
	return nil
}

func (w *BufferedPipeWriter) Write(src []byte) (int, error) {
	p := w.p
	if len(src) == 0 {
		p.mu.Lock()
		closed := p.rClosed || p.wClosed
		rErr := p.rErr
		p.mu.Unlock()
		if closed {
			if rErr != nil {
				return 0, rErr
			}
			return 0, io.ErrClosedPipe
		}
		return 0, nil
	}

	off := 0
	for off < len(src) {
		p.mu.Lock()
		if p.rClosed {
			err := p.rErr
			if err == nil {
				err = io.ErrClosedPipe
			}
			p.mu.Unlock()
			return off, err
		}
		if p.wClosed {
			p.mu.Unlock()
			return off, io.ErrClosedPipe
		}
		avail := p.maxBytes - p.size
		if avail <= 0 {
			p.spaceWaiters++
			p.mu.Unlock()
			select {
			case <-p.spaceNotify:
				p.mu.Lock()
				p.spaceWaiters--
				p.mu.Unlock()
				continue
			case <-p.rClosedCh:
				p.mu.Lock()
				p.spaceWaiters--
				p.mu.Unlock()
				continue
			case <-p.wClosedCh:
				p.mu.Lock()
				p.spaceWaiters--
				p.mu.Unlock()
				return off, io.ErrClosedPipe
			}
		}
		wasEmpty := p.size == 0
		chunk := len(src) - off
		if chunk > avail {
			chunk = avail
		}
		need := p.size + chunk
		if need > len(p.buf) {
			newCap := len(p.buf) * 2
			if newCap < 64<<10 {
				newCap = 64 << 10
			}
			for newCap < need {
				newCap *= 2
			}
			if newCap > p.maxBytes {
				newCap = p.maxBytes
			}
			newBuf := make([]byte, newCap)
			if p.size > 0 {
				first := len(p.buf) - p.head
				if first > p.size {
					first = p.size
				}
				copy(newBuf, p.buf[p.head:p.head+first])
				if first < p.size {
					copy(newBuf[first:], p.buf[:p.size-first])
				}
			}
			p.buf = newBuf
			p.head = 0
		}
		tail := (p.head + p.size) % len(p.buf)
		first := len(p.buf) - tail
		if first > chunk {
			first = chunk
		}
		copy(p.buf[tail:], src[off : off+first])
		if first < chunk {
			copy(p.buf, src[off+first : off+chunk])
		}
		p.size += chunk
		off += chunk
		p.mu.Unlock()

		if wasEmpty {
			select {
			case p.readNotify <- struct{}{}:
			default:
			}
		}
	}
	return off, nil
}

func (w *BufferedPipeWriter) Close() error {
	return w.CloseWithError(nil)
}

func (w *BufferedPipeWriter) CloseWithError(err error) error {
	p := w.p
	p.mu.Lock()
	if !p.wClosed {
		p.wClosed = true
		p.wErr = err
	}
	p.mu.Unlock()
	p.wCloseOnce.Do(func() {
		close(p.wClosedCh)
	})
	return nil
}
