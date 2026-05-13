package fronting

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// HTTPServerConn adapts an HTTP request/response pair into net.Conn.
type HTTPServerConn struct {
	body    io.ReadCloser
	writer  http.ResponseWriter
	flusher http.Flusher

	remote net.Addr
	local  net.Addr

	writeMu sync.Mutex
	closeMu sync.Mutex
	closed  bool

	bytesSinceFlush int
}

// NewHTTPServerConn returns a net.Conn backed by r.Body and w.
func NewHTTPServerConn(w http.ResponseWriter, r *http.Request) *HTTPServerConn {
	flusher, _ := w.(http.Flusher)
	return &HTTPServerConn{
		body:    r.Body,
		writer:  w,
		flusher: flusher,
		remote:  addrString(r.RemoteAddr),
		local:   addrString(r.Host),
	}
}

func (c *HTTPServerConn) Read(p []byte) (int, error) {
	return c.body.Read(p)
}

func (c *HTTPServerConn) Write(p []byte) (int, error) {
	c.closeMu.Lock()
	closed := c.closed
	c.closeMu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.writer.Write(p)
	c.bytesSinceFlush += n
	if c.shouldFlush(n) {
		c.flushLocked()
	}
	return n, err
}

func (c *HTTPServerConn) shouldFlush(lastWrite int) bool {
	if c.flusher == nil || lastWrite <= 0 {
		return false
	}
	return lastWrite < 4096 || c.bytesSinceFlush >= HTTPFlushThreshold
}

func (c *HTTPServerConn) flushLocked() {
	c.flusher.Flush()
	c.bytesSinceFlush = 0
}

func (c *HTTPServerConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.writeMu.Lock()
	if c.flusher != nil && c.bytesSinceFlush > 0 {
		c.flushLocked()
	}
	c.writeMu.Unlock()
	return c.body.Close()
}

func (c *HTTPServerConn) LocalAddr() net.Addr              { return c.local }
func (c *HTTPServerConn) RemoteAddr() net.Addr             { return c.remote }
func (c *HTTPServerConn) SetDeadline(time.Time) error      { return nil }
func (c *HTTPServerConn) SetReadDeadline(time.Time) error  { return nil }
func (c *HTTPServerConn) SetWriteDeadline(time.Time) error { return nil }

// HTTPClientConn adapts an HTTP response body and request pipe into net.Conn.
type HTTPClientConn struct {
	reader io.ReadCloser
	writer *io.PipeWriter
	respCh <-chan HTTPResponseResult

	remote net.Addr
	local  net.Addr

	initOnce sync.Once
	initErr  error

	closeOnce sync.Once
	closeErr  error
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
		reader: resp.Body,
		writer: writer,
		remote: addrString(remote),
		local:  addrString(""),
	}
}

// NewPendingHTTPClientConn returns a net.Conn that can write request body data
// before the HTTP response headers have arrived. Reads wait for the response.
func NewPendingHTTPClientConn(respCh <-chan HTTPResponseResult, writer *io.PipeWriter, remote string) *HTTPClientConn {
	return &HTTPClientConn{
		respCh: respCh,
		writer: writer,
		remote: addrString(remote),
		local:  addrString(""),
	}
}

func (c *HTTPClientConn) Read(p []byte) (int, error) {
	if err := c.ensureReader(); err != nil {
		return 0, err
	}
	return c.reader.Read(p)
}

func (c *HTTPClientConn) Write(p []byte) (int, error) {
	if c.writer == nil {
		return 0, net.ErrClosed
	}
	return c.writer.Write(p)
}

func (c *HTTPClientConn) Close() error {
	c.closeOnce.Do(func() {
		var errs []error
		if c.writer != nil {
			errs = append(errs, c.writer.Close())
		}
		if c.reader != nil {
			errs = append(errs, c.reader.Close())
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

func (c *HTTPClientConn) LocalAddr() net.Addr              { return c.local }
func (c *HTTPClientConn) RemoteAddr() net.Addr             { return c.remote }
func (c *HTTPClientConn) SetDeadline(time.Time) error      { return nil }
func (c *HTTPClientConn) SetReadDeadline(time.Time) error  { return nil }
func (c *HTTPClientConn) SetWriteDeadline(time.Time) error { return nil }

func (c *HTTPClientConn) ensureReader() error {
	if c.reader != nil {
		return nil
	}
	c.initOnce.Do(func() {
		if c.respCh == nil {
			c.initErr = net.ErrClosed
			return
		}
		result := <-c.respCh
		if result.Err != nil {
			c.initErr = result.Err
			return
		}
		if result.Response == nil || result.Response.Body == nil {
			c.initErr = io.ErrUnexpectedEOF
			return
		}
		c.reader = result.Response.Body
	})
	return c.initErr
}

type simpleAddr string

func (a simpleAddr) Network() string { return "fronting" }
func (a simpleAddr) String() string  { return string(a) }

func addrString(s string) net.Addr {
	return simpleAddr(s)
}
