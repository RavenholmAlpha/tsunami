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
	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	c.writing.Store(true)
	n, err := c.writer.Write(p)
	if n > 0 && c.flusher != nil {
		c.flusher.Flush()
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
	c.writeMu.Lock()
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

// HTTPClientConn adapts an HTTP response body and request pipe into net.Conn
// with real timer-driven read/write deadlines and leak-free cleanup.
type HTTPClientConn struct {
	reader    io.ReadCloser
	writer    *io.PipeWriter
	respCh    <-chan HTTPResponseResult
	reqCancel context.CancelFunc
	onClose   func()

	remote net.Addr
	local  net.Addr

	initOnce sync.Once
	initErr  error

	mu           sync.Mutex
	closed       bool
	closedCh     chan struct{}
	abortErr     error
	readTimer    *time.Timer
	writeTimer   *time.Timer
	readExpired  atomic.Bool
	writeExpired atomic.Bool
	closeOnce    sync.Once
	closeErr     error
}

// HTTPResponseResult carries a pending HTTP tunnel response.
type HTTPResponseResult struct {
	Response *http.Response
	Err      error
}

// NewHTTPClientConn returns a net.Conn backed by resp.Body and the request body pipe.
func NewHTTPClientConn(resp *http.Response, writer *io.PipeWriter) *HTTPClientConn {
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
func NewPendingHTTPClientConn(respCh <-chan HTTPResponseResult, writer *io.PipeWriter, remote string) *HTTPClientConn {
	return NewPendingHTTPClientConnWithCleanup(respCh, writer, remote, nil, nil)
}

// NewPendingHTTPClientConnWithCleanup creates an HTTPClientConn with request
// cancellation and transport cleanup hooks invoked on Close or deadline expiry.
func NewPendingHTTPClientConnWithCleanup(respCh <-chan HTTPResponseResult, writer *io.PipeWriter, remote string, cancel context.CancelFunc, onClose func()) *HTTPClientConn {
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
	c.closeOnce.Do(func() {
		c.abort(net.ErrClosed)
	})
	return c.closeErr
}

func (c *HTTPClientConn) abort(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if err == nil {
		err = net.ErrClosed
	}
	c.abortErr = err
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
	close(c.closedCh)
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
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}
	if t.IsZero() {
		c.readExpired.Store(false)
		c.mu.Unlock()
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		c.readExpired.Store(true)
		c.mu.Unlock()
		c.abort(os.ErrDeadlineExceeded)
		return nil
	}
	c.readExpired.Store(false)
	c.readTimer = time.AfterFunc(d, func() {
		c.readExpired.Store(true)
		c.abort(os.ErrDeadlineExceeded)
	})
	c.mu.Unlock()
	return nil
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
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
	if t.IsZero() {
		c.writeExpired.Store(false)
		c.mu.Unlock()
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		c.writeExpired.Store(true)
		c.mu.Unlock()
		c.abort(os.ErrDeadlineExceeded)
		return nil
	}
	c.writeExpired.Store(false)
	c.writeTimer = time.AfterFunc(d, func() {
		c.writeExpired.Store(true)
		c.abort(os.ErrDeadlineExceeded)
	})
	c.mu.Unlock()
	return nil
}

func (c *HTTPClientConn) ensureReader() error {
	if c.reader != nil {
		return nil
	}
	c.initOnce.Do(func() {
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
