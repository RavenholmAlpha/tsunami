package fronting

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxPendingSplitSessions = 256
	splitPairingTimeout     = 10 * time.Second
	maxSplitUplinkGap       = 16
	maxSplitUplinkBytes     = 2 << 20 // 2 MiB server-side uplink reorder buffer

	// MaxSplitChunkSize caps a single Split-H2 uplink POST body.
	MaxSplitChunkSize = 512 << 10

	splitClientMaxSendBuf  = 1 << 20   // 1 MiB client-side send buffer
	splitClientMaxChunk    = 128 << 10 // 128 KiB per POST chunk
	splitClientMaxInFlight = 4
)

var (
	errSplitSessionNotFound    = errors.New("fronting: split session not found")
	errDuplicateSplitDownlink  = errors.New("fronting: duplicate split downlink")
	errTooManyPendingSplitSess = errors.New("fronting: too many pending split sessions")
	errDuplicateSplitSeq       = errors.New("fronting: duplicate or stale uplink sequence")
	errSplitGapTooLarge        = errors.New("fronting: uplink sequence gap too large")
)

// SplitSessionRegistry pairs incoming Split-H2 GET downlinks and POST uplinks
// by session ID.
type SplitSessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*SplitH2ServerConn
	closed   bool
}

// NewSplitSessionRegistry creates a new Split-H2 session registry.
func NewSplitSessionRegistry() *SplitSessionRegistry {
	return &SplitSessionRegistry{
		sessions: make(map[string]*SplitH2ServerConn),
	}
}

// AttachDownlink binds the GET response stream for session sid.
func (r *SplitSessionRegistry) AttachDownlink(sid string, w http.ResponseWriter, req *http.Request) (*SplitH2ServerConn, error) {
	flusher, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, net.ErrClosed
	}

	s := r.sessions[sid]
	if s == nil {
		s = newSplitH2ServerConn(r, sid)
		r.sessions[sid] = s
	}
	r.mu.Unlock()

	if err := s.attachDownlink(w, flusher, rc, addrString(req.RemoteAddr), addrString(req.Host), req.Context()); err != nil {
		return nil, err
	}
	return s, nil
}

// DeliverUplink delivers an ordered uplink POST payload for session sid.
// If sid is not yet paired with a GET downlink, only seq == 0 may create a
// pending session (bounded by maxPendingSplitSessions and splitPairingTimeout).
func (r *SplitSessionRegistry) DeliverUplink(ctx context.Context, sid string, seq uint64, data []byte) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return net.ErrClosed
	}

	s := r.sessions[sid]
	if s == nil {
		if seq != 0 {
			r.mu.Unlock()
			return errSplitSessionNotFound
		}
		pending := 0
		for _, existing := range r.sessions {
			if !existing.isDownlinkAttached() {
				pending++
			}
		}
		if pending >= maxPendingSplitSessions {
			r.mu.Unlock()
			return errTooManyPendingSplitSess
		}
		s = newSplitH2ServerConn(r, sid)
		s.armPairTimeout(splitPairingTimeout)
		r.sessions[sid] = s
	}
	r.mu.Unlock()

	return s.enqueueUplink(ctx, seq, data)
}

// HasSession reports whether sid is currently registered.
func (r *SplitSessionRegistry) HasSession(sid string) bool {
	r.mu.Lock()
	_, ok := r.sessions[sid]
	r.mu.Unlock()
	return ok
}

func (r *SplitSessionRegistry) remove(sid string, conn *SplitH2ServerConn) {
	r.mu.Lock()
	if current, ok := r.sessions[sid]; ok && current == conn {
		delete(r.sessions, sid)
	}
	r.mu.Unlock()
}

// CloseAll closes every registered Split-H2 server connection.
func (r *SplitSessionRegistry) CloseAll() {
	r.mu.Lock()
	r.closed = true
	conns := make([]*SplitH2ServerConn, 0, len(r.sessions))
	for _, s := range r.sessions {
		conns = append(conns, s)
	}
	r.sessions = make(map[string]*SplitH2ServerConn)
	r.mu.Unlock()

	for _, s := range conns {
		_ = s.Close()
	}
}

// SplitH2ServerConn adapts a paired Split-H2 GET downlink + sequenced POST
// uplinks into a bidirectional net.Conn.
type SplitH2ServerConn struct {
	reg *SplitSessionRegistry
	sid string

	mu               sync.Mutex
	downlinkAttached bool
	pairTimer        *time.Timer
	writer           http.ResponseWriter
	flusher          http.Flusher
	rc               *http.ResponseController
	remote           net.Addr
	local            net.Addr

	nextSeq       uint64
	pending       map[uint64][]byte
	readQueue     [][]byte
	readLeft      []byte
	bufferedBytes int

	closed       bool
	closedCh     chan struct{}
	readNotify   chan struct{}
	spaceNotify  chan struct{}
	readTimer    *time.Timer
	stopCtxWatch func() bool
	readExpired  atomic.Bool
	writeMu      sync.Mutex
	writing      atomic.Bool
	closedWrite  atomic.Bool
	closeOnce    sync.Once
}

func newSplitH2ServerConn(reg *SplitSessionRegistry, sid string) *SplitH2ServerConn {
	return &SplitH2ServerConn{
		reg:         reg,
		sid:         sid,
		pending:     make(map[uint64][]byte),
		closedCh:    make(chan struct{}),
		readNotify:  make(chan struct{}, 1),
		spaceNotify: make(chan struct{}, 1),
		remote:      addrString(""),
		local:       addrString(""),
	}
}

func (c *SplitH2ServerConn) isDownlinkAttached() bool {
	c.mu.Lock()
	attached := c.downlinkAttached
	c.mu.Unlock()
	return attached
}

func (c *SplitH2ServerConn) armPairTimeout(d time.Duration) {
	c.mu.Lock()
	if !c.downlinkAttached && !c.closed {
		c.pairTimer = time.AfterFunc(d, func() {
			c.mu.Lock()
			expired := !c.downlinkAttached && !c.closed
			c.mu.Unlock()
			if expired {
				_ = c.Close()
			}
		})
	}
	c.mu.Unlock()
}

func (c *SplitH2ServerConn) attachDownlink(w http.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, remote, local net.Addr, reqCtx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return net.ErrClosed
	}
	if c.downlinkAttached {
		c.mu.Unlock()
		return errDuplicateSplitDownlink
	}
	if c.pairTimer != nil {
		c.pairTimer.Stop()
		c.pairTimer = nil
	}
	c.downlinkAttached = true
	c.writer = w
	c.flusher = flusher
	c.rc = rc
	c.remote = remote
	c.local = local
	if reqCtx != nil {
		c.stopCtxWatch = context.AfterFunc(reqCtx, func() {
			_ = c.Close()
		})
	}
	c.mu.Unlock()
	return nil
}

func (c *SplitH2ServerConn) enqueueUplink(ctx context.Context, seq uint64, data []byte) error {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return net.ErrClosed
		}
		if seq < c.nextSeq {
			c.mu.Unlock()
			return errDuplicateSplitSeq
		}
		if _, exists := c.pending[seq]; exists {
			c.mu.Unlock()
			return errDuplicateSplitSeq
		}
		if seq > c.nextSeq+maxSplitUplinkGap {
			c.mu.Unlock()
			return errSplitGapTooLarge
		}
		if c.bufferedBytes > 0 && c.bufferedBytes+len(data) > maxSplitUplinkBytes {
			c.mu.Unlock()
			select {
			case <-c.spaceNotify:
				continue
			case <-c.closedCh:
				return net.ErrClosed
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		c.pending[seq] = data
		c.bufferedBytes += len(data)
		for {
			chunk, ok := c.pending[c.nextSeq]
			if !ok {
				break
			}
			delete(c.pending, c.nextSeq)
			c.nextSeq++
			if len(chunk) > 0 {
				c.readQueue = append(c.readQueue, chunk)
			}
		}
		c.mu.Unlock()

		select {
		case c.readNotify <- struct{}{}:
		default:
		}
		return nil
	}
}

func (c *SplitH2ServerConn) Read(p []byte) (int, error) {
	if c.readExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	if len(c.readLeft) > 0 {
		n := copy(p, c.readLeft)
		c.readLeft = c.readLeft[n:]
		return n, nil
	}

	for {
		c.mu.Lock()
		if c.readExpired.Load() {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if len(c.readQueue) > 0 {
			chunk := c.readQueue[0]
			c.readQueue[0] = nil
			c.readQueue = c.readQueue[1:]
			if len(c.readQueue) == 0 {
				c.readQueue = nil
			}
			c.bufferedBytes -= len(chunk)
			c.mu.Unlock()

			select {
			case c.spaceNotify <- struct{}{}:
			default:
			}

			n := copy(p, chunk)
			if n < len(chunk) {
				c.readLeft = chunk[n:]
			}
			return n, nil
		}
		if c.closed {
			c.mu.Unlock()
			return 0, io.EOF
		}
		c.mu.Unlock()

		select {
		case <-c.readNotify:
		case <-c.closedCh:
		}
	}
}

func (c *SplitH2ServerConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closedWrite.Load() || c.writer == nil {
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

func (c *SplitH2ServerConn) Close() error {
	c.closeOnce.Do(func() {
		c.closedWrite.Store(true)

		c.mu.Lock()
		c.closed = true
		if c.pairTimer != nil {
			c.pairTimer.Stop()
			c.pairTimer = nil
		}
		if c.readTimer != nil {
			c.readTimer.Stop()
			c.readTimer = nil
		}
		stopWatch := c.stopCtxWatch
		c.stopCtxWatch = nil
		c.pending = nil
		c.readQueue = nil
		c.bufferedBytes = 0
		rc := c.rc
		close(c.closedCh)
		c.mu.Unlock()

		if stopWatch != nil {
			stopWatch()
		}
		if c.writing.Load() && rc != nil {
			_ = rc.SetWriteDeadline(time.Unix(1, 0))
		}
		// Wait for any in-flight ResponseWriter.Write/Flush to finish and clear writer refs.
		c.writeMu.Lock()
		c.writer = nil
		c.flusher = nil
		c.writeMu.Unlock()

		if c.reg != nil {
			c.reg.remove(c.sid, c)
		}
	})
	return nil
}

func (c *SplitH2ServerConn) LocalAddr() net.Addr  { return c.local }
func (c *SplitH2ServerConn) RemoteAddr() net.Addr { return c.remote }

func (c *SplitH2ServerConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *SplitH2ServerConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
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
		select {
		case c.readNotify <- struct{}{}:
		default:
		}
		return nil
	}
	c.readExpired.Store(false)
	c.readTimer = time.AfterFunc(d, func() {
		c.readExpired.Store(true)
		select {
		case c.readNotify <- struct{}{}:
		default:
		}
	})
	c.mu.Unlock()
	return nil
}

func (c *SplitH2ServerConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	closed := c.closed
	rc := c.rc
	c.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	if rc == nil {
		return nil
	}
	err := rc.SetWriteDeadline(t)
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// SplitH2ClientConn implements net.Conn over a single HTTP/2 connection carrying
// one streaming GET downlink and pipelined finite-Content-Length POST uplinks.
type SplitH2ClientConn struct {
	endpoint     *url.URL
	host         string
	key          [32]byte
	fingerprint  string
	sid          string
	roundTripper func(*http.Request) (*http.Response, error)
	onClose      func()

	reqCtx    context.Context
	reqCancel context.CancelFunc

	respCh   chan HTTPResponseResult
	reader   io.ReadCloser
	initOnce sync.Once
	initErr  error

	mu           sync.Mutex
	closed       bool
	closedCh     chan struct{}
	abortErr     error
	sendBuf      []byte
	nextSeq      uint64
	sendNotify   chan struct{}
	spaceNotify  chan struct{}
	readTimer    *time.Timer
	writeTimer   *time.Timer
	readExpired  atomic.Bool
	writeExpired atomic.Bool
	closeOnce    sync.Once
}

// NewSplitH2ClientConn creates and starts a Split-H2 client connection using the
// provided HTTP/2 round-tripper function.
func NewSplitH2ClientConn(
	endpoint *url.URL,
	host string,
	key [32]byte,
	fingerprint string,
	roundTripper func(*http.Request) (*http.Response, error),
	onClose func(),
) (*SplitH2ClientConn, error) {
	if endpoint == nil {
		return nil, errors.New("fronting: split h2 endpoint is required")
	}
	if roundTripper == nil {
		return nil, errors.New("fronting: split h2 roundTripper is required")
	}

	var sidRaw [16]byte
	if _, err := rand.Read(sidRaw[:]); err != nil {
		return nil, fmt.Errorf("fronting: split session id: %w", err)
	}
	sid := hex.EncodeToString(sidRaw[:])
	if host == "" {
		host = endpoint.Host
	}

	reqCtx, reqCancel := context.WithCancel(context.Background())
	c := &SplitH2ClientConn{
		endpoint:     endpoint,
		host:         host,
		key:          key,
		fingerprint:  fingerprint,
		sid:          sid,
		roundTripper: roundTripper,
		onClose:      onClose,
		reqCtx:       reqCtx,
		reqCancel:    reqCancel,
		respCh:       make(chan HTTPResponseResult, 1),
		closedCh:     make(chan struct{}),
		sendNotify:   make(chan struct{}, 1),
		spaceNotify:  make(chan struct{}, 1),
	}

	c.startDownlink()
	go c.uploadLoop()

	return c, nil
}

func (c *SplitH2ClientConn) startDownlink() {
	req, err := http.NewRequestWithContext(c.reqCtx, http.MethodGet, c.endpoint.String(), nil)
	if err != nil {
		c.respCh <- HTTPResponseResult{Err: err}
		return
	}
	req.Host = c.host
	ApplyBrowserHeaders(req.Header, c.fingerprint)
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set(HeaderSessionID, c.sid)
	if err := SignRequest(req, c.key, time.Now()); err != nil {
		c.respCh <- HTTPResponseResult{Err: err}
		return
	}

	go func() {
		resp, err := c.roundTripper(req)
		if err != nil {
			c.respCh <- HTTPResponseResult{Err: fmt.Errorf("fronting: split h2 downlink: %w", err)}
			return
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			c.respCh <- HTTPResponseResult{Err: fmt.Errorf("fronting: split h2 downlink status %s", resp.Status)}
			return
		}
		if resp.ProtoMajor < 2 {
			resp.Body.Close()
			c.respCh <- HTTPResponseResult{Err: fmt.Errorf("fronting: expected HTTP/2, got %s", resp.Proto)}
			return
		}
		c.respCh <- HTTPResponseResult{Response: resp}
		if c.IsClosed() {
			_ = resp.Body.Close()
		}
	}()
}

// IsClosed reports whether the connection is closed or aborted.
func (c *SplitH2ClientConn) IsClosed() bool {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	return closed
}

func (c *SplitH2ClientConn) Read(p []byte) (int, error) {
	if err := c.ensureReader(); err != nil {
		return 0, err
	}
	n, err := c.reader.Read(p)
	if err != nil && c.readExpired.Load() {
		return n, os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *SplitH2ClientConn) ensureReader() error {
	if c.reader != nil {
		return nil
	}
	c.initOnce.Do(func() {
		select {
		case res := <-c.respCh:
			if res.Err != nil {
				c.initErr = res.Err
				return
			}
			if res.Response == nil || res.Response.Body == nil {
				c.initErr = io.ErrUnexpectedEOF
				return
			}
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				_ = res.Response.Body.Close()
				if c.readExpired.Load() {
					c.initErr = os.ErrDeadlineExceeded
				} else {
					c.initErr = net.ErrClosed
				}
				return
			}
			c.reader = res.Response.Body
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

func (c *SplitH2ClientConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		if c.IsClosed() {
			if c.writeExpired.Load() {
				return 0, os.ErrDeadlineExceeded
			}
			return 0, net.ErrClosed
		}
		return 0, nil
	}

	off := 0
	for off < len(p) {
		c.mu.Lock()
		if c.writeExpired.Load() {
			c.mu.Unlock()
			return off, os.ErrDeadlineExceeded
		}
		if c.closed {
			err := c.abortErr
			if err == nil {
				err = net.ErrClosed
			}
			c.mu.Unlock()
			return off, err
		}

		avail := splitClientMaxSendBuf - len(c.sendBuf)
		if avail <= 0 {
			c.mu.Unlock()
			select {
			case <-c.spaceNotify:
				continue
			case <-c.closedCh:
				if c.writeExpired.Load() {
					return off, os.ErrDeadlineExceeded
				}
				return off, net.ErrClosed
			}
		}

		chunk := len(p) - off
		if chunk > avail {
			chunk = avail
		}
		c.sendBuf = append(c.sendBuf, p[off:off+chunk]...)
		off += chunk
		c.mu.Unlock()

		select {
		case c.sendNotify <- struct{}{}:
		default:
		}
	}

	return off, nil
}

func (c *SplitH2ClientConn) uploadLoop() {
	sem := make(chan struct{}, splitClientMaxInFlight)

	for {
		chunk, seq, ok := c.nextUploadChunk()
		if !ok {
			return
		}

		// Send seq == 0 synchronously before pipelining so session creation at
		// the server always sees seq == 0 before any seq > 0 arrives.
		if seq == 0 {
			if err := c.sendChunk(seq, chunk); err != nil {
				c.abort(err)
				return
			}
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-c.closedCh:
			return
		}

		go func(seqNum uint64, data []byte) {
			defer func() { <-sem }()
			if err := c.sendChunk(seqNum, data); err != nil {
				c.abort(err)
			}
		}(seq, chunk)
	}
}

func (c *SplitH2ClientConn) nextUploadChunk() ([]byte, uint64, bool) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, 0, false
		}
		if len(c.sendBuf) > 0 {
			n := len(c.sendBuf)
			if n > splitClientMaxChunk {
				n = splitClientMaxChunk
			}
			chunk := make([]byte, n)
			copy(chunk, c.sendBuf[:n])
			copy(c.sendBuf, c.sendBuf[n:])
			c.sendBuf = c.sendBuf[:len(c.sendBuf)-n]
			seq := c.nextSeq
			c.nextSeq++
			c.mu.Unlock()

			select {
			case c.spaceNotify <- struct{}{}:
			default:
			}
			return chunk, seq, true
		}
		c.mu.Unlock()

		select {
		case <-c.sendNotify:
		case <-c.closedCh:
			return nil, 0, false
		}
	}
}

func (c *SplitH2ClientConn) sendChunk(seq uint64, data []byte) error {
	req, err := http.NewRequestWithContext(c.reqCtx, http.MethodPost, c.endpoint.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Host = c.host
	ApplyBrowserHeaders(req.Header, c.fingerprint)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set(HeaderSessionID, c.sid)
	req.Header.Set(HeaderStreamSeq, strconv.FormatUint(seq, 10))
	if err := SignRequest(req, c.key, time.Now()); err != nil {
		return err
	}

	resp, err := c.roundTripper(req)
	if err != nil {
		return fmt.Errorf("fronting: split h2 uplink seq=%d: %w", seq, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fronting: split h2 uplink seq=%d status %s", seq, resp.Status)
	}
	if resp.ProtoMajor < 2 {
		return fmt.Errorf("fronting: split h2 uplink seq=%d expected HTTP/2, got %s", seq, resp.Proto)
	}
	return nil
}

func (c *SplitH2ClientConn) abort(err error) {
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
	c.sendBuf = nil
	close(c.closedCh)
	reader := c.reader
	cancel := c.reqCancel
	onClose := c.onClose
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if reader != nil {
		_ = reader.Close()
	}
	select {
	case res := <-c.respCh:
		if res.Response != nil && res.Response.Body != nil {
			_ = res.Response.Body.Close()
		}
	default:
	}
	if onClose != nil {
		onClose()
	}
}

func (c *SplitH2ClientConn) Close() error {
	c.closeOnce.Do(func() {
		c.abort(net.ErrClosed)
	})
	return nil
}

func (c *SplitH2ClientConn) LocalAddr() net.Addr  { return addrString("") }
func (c *SplitH2ClientConn) RemoteAddr() net.Addr { return addrString(c.endpoint.Host) }

func (c *SplitH2ClientConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *SplitH2ClientConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		err := c.abortErr
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

func (c *SplitH2ClientConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	if c.closed {
		err := c.abortErr
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
