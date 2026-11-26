package fronting

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
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
	maxSplitUplinkGap       = 48
	maxSplitUplinkBytes     = 16 << 20 // 16 MiB server-side uplink reorder buffer

	// MaxSplitChunkSize caps a single Split-H2 uplink POST body.
	MaxSplitChunkSize = 512 << 10

	splitServerSmallCap = 4 << 10
	splitServerMidCap   = 32 << 10
	splitServerLargeCap = MaxSplitChunkSize

	splitClientMaxSendBuf  = 12 << 20  // 12 MiB client-side send buffer
	splitClientMaxChunk    = 512 << 10 // 512 KiB per POST chunk (matches MaxSplitChunkSize)
	splitClientMaxInFlight = 24
	splitCoalesceDelay     = 300 * time.Microsecond
)

var (
	splitServerSmallPool = sync.Pool{
		New: func() any {
			buf := make([]byte, splitServerSmallCap)
			return &buf
		},
	}
	splitServerMidPool = sync.Pool{
		New: func() any {
			buf := make([]byte, splitServerMidCap)
			return &buf
		},
	}
	splitServerLargePool = sync.Pool{
		New: func() any {
			buf := make([]byte, splitServerLargeCap)
			return &buf
		},
	}
	splitClientChunkPool = sync.Pool{
		New: func() any {
			buf := make([]byte, splitClientMaxChunk)
			return &buf
		},
	}
)

type splitChunk struct {
	data    []byte
	poolBuf *[]byte
}

func getServerSplitChunk(size int) *[]byte {
	switch {
	case size <= splitServerSmallCap:
		return splitServerSmallPool.Get().(*[]byte)
	case size <= splitServerMidCap:
		return splitServerMidPool.Get().(*[]byte)
	default:
		return splitServerLargePool.Get().(*[]byte)
	}
}

func recycleServerSplitChunk(poolBuf *[]byte) {
	if poolBuf == nil {
		return
	}
	switch cap(*poolBuf) {
	case splitServerSmallCap:
		*poolBuf = (*poolBuf)[:splitServerSmallCap]
		splitServerSmallPool.Put(poolBuf)
	case splitServerMidCap:
		*poolBuf = (*poolBuf)[:splitServerMidCap]
		splitServerMidPool.Put(poolBuf)
	case splitServerLargeCap:
		*poolBuf = (*poolBuf)[:splitServerLargeCap]
		splitServerLargePool.Put(poolBuf)
	}
}

func recycleClientSplitChunk(poolBuf *[]byte) {
	if poolBuf != nil && cap(*poolBuf) >= splitClientMaxChunk {
		*poolBuf = (*poolBuf)[:splitClientMaxChunk]
		splitClientChunkPool.Put(poolBuf)
	}
}

// ReadSplitUplinkBody reads a bounded Split-H2 uplink POST body, using tiered
// pools (4 KiB / 32 KiB / 512 KiB) to avoid slice growth and memory bloat.
func ReadSplitUplinkBody(body io.Reader, contentLength int64) ([]byte, *[]byte, error) {
	if contentLength > MaxSplitChunkSize {
		return nil, nil, fmt.Errorf("fronting: uplink chunk exceeds max size %d", contentLength)
	}
	if contentLength == 0 {
		return nil, nil, nil
	}
	if contentLength > 0 {
		bufp := getServerSplitChunk(int(contentLength))
		data := (*bufp)[:int(contentLength)]
		if _, err := io.ReadFull(body, data); err != nil {
			recycleServerSplitChunk(bufp)
			return nil, nil, err
		}
		return data, bufp, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxSplitChunkSize+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxSplitChunkSize {
		return nil, nil, errors.New("fronting: uplink chunk exceeds max size")
	}
	return data, nil, nil
}

// RecycleSplitUplinkBody returns a buffer obtained from ReadSplitUplinkBody if it
// was not transferred to DeliverUplinkPooled.
func RecycleSplitUplinkBody(poolBuf *[]byte) {
	recycleServerSplitChunk(poolBuf)
}

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
	mu           sync.RWMutex
	sessions     map[string]*SplitH2ServerConn
	closed       bool
	pendingCount atomic.Int32
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
	return r.deliverUplinkChunk(ctx, sid, seq, splitChunk{data: data})
}

// DeliverUplinkPooled delivers a pooled uplink chunk and takes ownership of
// poolBuf when returning nil; on error poolBuf is recycled immediately.
func (r *SplitSessionRegistry) DeliverUplinkPooled(ctx context.Context, sid string, seq uint64, data []byte, poolBuf *[]byte) error {
	err := r.deliverUplinkChunk(ctx, sid, seq, splitChunk{data: data, poolBuf: poolBuf})
	if err != nil && poolBuf != nil {
		recycleServerSplitChunk(poolBuf)
	}
	return err
}

func (r *SplitSessionRegistry) deliverUplinkChunk(ctx context.Context, sid string, seq uint64, chunk splitChunk) error {
	// Fast path: steady-state uplink POSTs hit an existing session under RLock.
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return net.ErrClosed
	}
	s := r.sessions[sid]
	r.mu.RUnlock()
	if s != nil {
		return s.enqueueUplink(ctx, seq, chunk)
	}

	if seq != 0 {
		return errSplitSessionNotFound
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return net.ErrClosed
	}
	s = r.sessions[sid]
	if s == nil {
		if r.pendingCount.Load() >= maxPendingSplitSessions {
			r.mu.Unlock()
			return errTooManyPendingSplitSess
		}
		s = newSplitH2ServerConn(r, sid)
		s.pendingTracked = true
		r.pendingCount.Add(1)
		s.armPairTimeout(splitPairingTimeout)
		r.sessions[sid] = s
	}
	r.mu.Unlock()

	return s.enqueueUplink(ctx, seq, chunk)
}

// HasSession reports whether sid is currently registered.
func (r *SplitSessionRegistry) HasSession(sid string) bool {
	r.mu.RLock()
	_, ok := r.sessions[sid]
	r.mu.RUnlock()
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
	pendingTracked   bool
	pairTimer        *time.Timer
	writer           http.ResponseWriter
	flusher          http.Flusher
	rc               *http.ResponseController
	remote           net.Addr
	local            net.Addr

	nextSeq       uint64
	pending       map[uint64]splitChunk
	readQueue     []splitChunk
	readQueueHead int
	readLeftChunk splitChunk
	bufferedBytes int
	spaceWaiters  int

	closed             bool
	closedCh           chan struct{}
	readNotify         chan struct{}
	spaceNotify        chan struct{}
	readTimer          *time.Timer
	readDeadlineAt     time.Time
	readDeadlineActive bool
	stopCtxWatch       func() bool
	readExpired        atomic.Bool
	writeMu            sync.Mutex
	writing            atomic.Bool
	closedWrite        atomic.Bool
	closeOnce          sync.Once
}

func newSplitH2ServerConn(reg *SplitSessionRegistry, sid string) *SplitH2ServerConn {
	return &SplitH2ServerConn{
		reg:         reg,
		sid:         sid,
		closedCh:    make(chan struct{}),
		readNotify:  make(chan struct{}, 1),
		spaceNotify: make(chan struct{}, 1),
		remote:      addrString(""),
		local:       addrString(""),
	}
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
	wasPending := c.pendingTracked
	c.pendingTracked = false
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
	if wasPending && c.reg != nil {
		c.reg.pendingCount.Add(-1)
	}
	return nil
}

func (c *SplitH2ServerConn) enqueueUplink(ctx context.Context, seq uint64, chunk splitChunk) error {
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
		if seq > c.nextSeq {
			if seq > c.nextSeq+maxSplitUplinkGap {
				c.mu.Unlock()
				return errSplitGapTooLarge
			}
			if c.pending != nil {
				if _, exists := c.pending[seq]; exists {
					c.mu.Unlock()
					return errDuplicateSplitSeq
				}
			}
		}
		if c.bufferedBytes > 0 && c.bufferedBytes+len(chunk.data) > maxSplitUplinkBytes {
			c.spaceWaiters++
			c.mu.Unlock()
			select {
			case <-c.spaceNotify:
				c.mu.Lock()
				c.spaceWaiters--
				c.mu.Unlock()
				continue
			case <-c.closedCh:
				c.mu.Lock()
				c.spaceWaiters--
				c.mu.Unlock()
				return net.ErrClosed
			case <-ctx.Done():
				c.mu.Lock()
				c.spaceWaiters--
				c.mu.Unlock()
				return ctx.Err()
			}
		}

		// In-order fast path: skip pending map insertion and lookup completely.
		if seq == c.nextSeq {
			wasEmpty := c.readQueueHead == len(c.readQueue)
			c.nextSeq++
			if len(chunk.data) > 0 {
				c.bufferedBytes += len(chunk.data)
				c.readQueue = append(c.readQueue, chunk)
			} else if chunk.poolBuf != nil {
				recycleServerSplitChunk(chunk.poolBuf)
			}
			for len(c.pending) > 0 {
				ready, ok := c.pending[c.nextSeq]
				if !ok {
					break
				}
				delete(c.pending, c.nextSeq)
				c.nextSeq++
				if len(ready.data) > 0 {
					c.readQueue = append(c.readQueue, ready)
				} else if ready.poolBuf != nil {
					recycleServerSplitChunk(ready.poolBuf)
				}
			}
			notifyRead := wasEmpty && c.readQueueHead < len(c.readQueue)
			c.mu.Unlock()

			if notifyRead {
				select {
				case c.readNotify <- struct{}{}:
				default:
				}
			}
			return nil
		}

		// Out-of-order chunk: buffer in pending until nextSeq catches up.
		if c.pending == nil {
			c.pending = make(map[uint64]splitChunk, 8)
		}
		c.pending[seq] = chunk
		c.bufferedBytes += len(chunk.data)
		c.mu.Unlock()
		return nil
	}
}

func (c *SplitH2ServerConn) Read(p []byte) (int, error) {
	if c.readExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	if len(p) == 0 {
		return 0, nil
	}

	for {
		c.mu.Lock()
		if c.readExpired.Load() {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		total := 0
		poppedQueue := false
		if len(c.readLeftChunk.data) > 0 {
			n := copy(p, c.readLeftChunk.data)
			c.readLeftChunk.data = c.readLeftChunk.data[n:]
			if len(c.readLeftChunk.data) == 0 && c.readLeftChunk.poolBuf != nil {
				recycleServerSplitChunk(c.readLeftChunk.poolBuf)
				c.readLeftChunk.poolBuf = nil
			}
			total += n
		}
		for total < len(p) && c.readQueueHead < len(c.readQueue) {
			chunk := c.readQueue[c.readQueueHead]
			c.readQueue[c.readQueueHead] = splitChunk{}
			c.readQueueHead++
			c.bufferedBytes -= len(chunk.data)
			poppedQueue = true

			n := copy(p[total:], chunk.data)
			if n < len(chunk.data) {
				c.readLeftChunk = splitChunk{data: chunk.data[n:], poolBuf: chunk.poolBuf}
			} else if chunk.poolBuf != nil {
				recycleServerSplitChunk(chunk.poolBuf)
			}
			total += n
		}
		if c.readQueueHead == len(c.readQueue) {
			c.readQueue = c.readQueue[:0]
			c.readQueueHead = 0
		} else if c.readQueueHead >= 32 && c.readQueueHead*2 >= len(c.readQueue) {
			rem := copy(c.readQueue, c.readQueue[c.readQueueHead:])
			clear(c.readQueue[rem:])
			c.readQueue = c.readQueue[:rem]
			c.readQueueHead = 0
		}
		if total > 0 {
			notifySpace := poppedQueue && c.spaceWaiters > 0
			c.mu.Unlock()
			if notifySpace {
				select {
				case c.spaceNotify <- struct{}{}:
				default:
				}
			}
			return total, nil
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

func (c *SplitH2ServerConn) Close() error {
	c.closeOnce.Do(func() {
		c.closedWrite.Store(true)

		c.mu.Lock()
		c.closed = true
		wasPending := c.pendingTracked
		c.pendingTracked = false
		c.readDeadlineActive = false
		if c.pairTimer != nil {
			c.pairTimer.Stop()
			c.pairTimer = nil
		}
		if c.readTimer != nil {
			c.readTimer.Stop()
		}
		stopWatch := c.stopCtxWatch
		c.stopCtxWatch = nil
		pending := c.pending
		c.pending = nil
		var queued []splitChunk
		if c.readQueueHead < len(c.readQueue) {
			queued = c.readQueue[c.readQueueHead:]
		}
		leftBuf := c.readLeftChunk.poolBuf
		c.readLeftChunk = splitChunk{}
		c.readQueue = nil
		c.readQueueHead = 0
		c.bufferedBytes = 0
		rc := c.rc
		close(c.closedCh)
		c.mu.Unlock()

		if wasPending && c.reg != nil {
			c.reg.pendingCount.Add(-1)
		}

		if leftBuf != nil {
			recycleServerSplitChunk(leftBuf)
		}

		for _, ch := range pending {
			if ch.poolBuf != nil {
				recycleServerSplitChunk(ch.poolBuf)
			}
		}
		for _, ch := range queued {
			if ch.poolBuf != nil {
				recycleServerSplitChunk(ch.poolBuf)
			}
		}

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
		select {
		case c.readNotify <- struct{}{}:
		default:
		}
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

func (c *SplitH2ServerConn) onReadDeadline() {
	c.mu.Lock()
	if c.closed || !c.readDeadlineActive || time.Now().Before(c.readDeadlineAt) {
		c.mu.Unlock()
		return
	}
	c.readDeadlineActive = false
	c.readExpired.Store(true)
	c.mu.Unlock()
	select {
	case c.readNotify <- struct{}{}:
	default:
	}
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
	sidHdr       []string
	roundTripper func(*http.Request) (*http.Response, error)
	onClose      func()

	nonceMu  sync.Mutex
	nonceRng *mrand.ChaCha8

	reqCtx    context.Context
	reqCancel context.CancelFunc

	respCh   chan HTTPResponseResult
	reader   io.ReadCloser
	initOnce sync.Once
	initErr  error

	inFlight atomic.Int32

	mu                  sync.Mutex
	closed              bool
	closedCh            chan struct{}
	abortErr            error
	sendChunks          []clientSendChunk
	sendChunksHead      int
	sendLen             int
	spaceWaiters        int
	nextSeq             uint64
	sendNotify          chan struct{}
	spaceNotify         chan struct{}
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

	var sidAndSeed [48]byte
	if _, err := rand.Read(sidAndSeed[:]); err != nil {
		return nil, fmt.Errorf("fronting: split session id: %w", err)
	}
	sid := hex.EncodeToString(sidAndSeed[:16])
	var seed [32]byte
	copy(seed[:], sidAndSeed[16:])
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
		sidHdr:       []string{sid},
		roundTripper: roundTripper,
		onClose:      onClose,
		nonceRng:     mrand.NewChaCha8(seed),
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

func (c *SplitH2ClientConn) nextRawNonce() [16]byte {
	var raw [16]byte
	c.nonceMu.Lock()
	binary.LittleEndian.PutUint64(raw[0:8], c.nonceRng.Uint64())
	binary.LittleEndian.PutUint64(raw[8:16], c.nonceRng.Uint64())
	c.nonceMu.Unlock()
	return raw
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
	req.Header[HeaderSessionID] = c.sidHdr
	SignRequestWithRawNonce(req, c.key, time.Now(), c.nextRawNonce())

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

		avail := splitClientMaxSendBuf - c.sendLen
		if avail <= 0 {
			c.spaceWaiters++
			c.mu.Unlock()
			select {
			case <-c.spaceNotify:
				c.mu.Lock()
				c.spaceWaiters--
				c.mu.Unlock()
				continue
			case <-c.closedCh:
				c.mu.Lock()
				c.spaceWaiters--
				c.mu.Unlock()
				if c.writeExpired.Load() {
					return off, os.ErrDeadlineExceeded
				}
				return off, net.ErrClosed
			}
		}

		prevChunks := len(c.sendChunks) - c.sendChunksHead
		chunk := len(p) - off
		if chunk > avail {
			chunk = avail
		}
		c.appendSendChunksLocked(p[off : off+chunk])
		off += chunk
		curChunks := len(c.sendChunks) - c.sendChunksHead
		notifySend := prevChunks == 0 || curChunks > prevChunks ||
			(curChunks == 1 && c.sendChunks[c.sendChunksHead].len >= splitClientMaxChunk)
		c.mu.Unlock()

		if notifySend {
			select {
			case c.sendNotify <- struct{}{}:
			default:
			}
		}
	}

	return off, nil
}

type clientSendChunk struct {
	bufp *[]byte
	len  int
}

func (c *SplitH2ClientConn) appendSendChunksLocked(src []byte) {
	if c.sendChunksHead >= 32 && c.sendChunksHead*2 >= len(c.sendChunks) {
		rem := copy(c.sendChunks, c.sendChunks[c.sendChunksHead:])
		clear(c.sendChunks[rem:])
		c.sendChunks = c.sendChunks[:rem]
		c.sendChunksHead = 0
	}
	for len(src) > 0 {
		if c.sendChunksHead < len(c.sendChunks) {
			tail := &c.sendChunks[len(c.sendChunks)-1]
			if tail.len < splitClientMaxChunk {
				n := copy((*tail.bufp)[tail.len:splitClientMaxChunk], src)
				tail.len += n
				c.sendLen += n
				src = src[n:]
				if len(src) == 0 {
					return
				}
			}
		}
		bufp := splitClientChunkPool.Get().(*[]byte)
		n := copy((*bufp)[:splitClientMaxChunk], src)
		c.sendChunks = append(c.sendChunks, clientSendChunk{bufp: bufp, len: n})
		c.sendLen += n
		src = src[n:]
	}
}

type splitUploadJob struct {
	seq  uint64
	bufp *[]byte
	n    int
}

func (c *SplitH2ClientConn) uploadLoop() {
	jobCh := make(chan splitUploadJob)
	var workers int

	coalesceTimer := time.NewTimer(splitCoalesceDelay)
	if !coalesceTimer.Stop() {
		select {
		case <-coalesceTimer.C:
		default:
		}
	}
	defer coalesceTimer.Stop()

	runJob := func(job splitUploadJob) error {
		defer c.inFlight.Add(-1)
		return c.sendChunk(job.seq, job.bufp, job.n)
	}

	for {
		bufp, n, seq, ok := c.nextUploadChunk(coalesceTimer)
		if !ok {
			close(jobCh)
			return
		}

		// Send seq == 0 synchronously before pipelining so session creation at
		// the server always sees seq == 0 before any seq > 0 arrives.
		if seq == 0 {
			if err := c.sendChunk(seq, bufp, n); err != nil {
				close(jobCh)
				c.abort(err)
				return
			}
			continue
		}

		job := splitUploadJob{seq: seq, bufp: bufp, n: n}
		c.inFlight.Add(1)
		select {
		case jobCh <- job:
			continue
		default:
		}

		if workers < splitClientMaxInFlight {
			workers++
			go func(first splitUploadJob) {
				if err := runJob(first); err != nil {
					c.abort(err)
					return
				}
				for next := range jobCh {
					if err := runJob(next); err != nil {
						c.abort(err)
						return
					}
				}
			}(job)
			continue
		}

		select {
		case jobCh <- job:
		case <-c.closedCh:
			c.inFlight.Add(-1)
			recycleClientSplitChunk(job.bufp)
			close(jobCh)
			return
		}
	}
}

func (c *SplitH2ClientConn) nextUploadChunk(coalesceTimer *time.Timer) (*[]byte, int, uint64, bool) {
	coalesced := false
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, 0, 0, false
		}
		queuedChunks := len(c.sendChunks) - c.sendChunksHead
		if queuedChunks > 0 {
			front := c.sendChunks[c.sendChunksHead]
			// Micro-coalesce partial chunks ONLY while at least one POST is already
			// in flight (0us added latency on idle/interactive writes and seq == 0).
			if !coalesced && c.nextSeq > 0 && queuedChunks == 1 && front.len < splitClientMaxChunk && c.inFlight.Load() > 0 {
				select {
				case <-c.sendNotify:
				default:
				}
				c.mu.Unlock()
				coalesced = true
				coalesceTimer.Reset(splitCoalesceDelay)
				select {
				case <-coalesceTimer.C:
				case <-c.sendNotify:
					if !coalesceTimer.Stop() {
						select {
						case <-coalesceTimer.C:
						default:
						}
					}
				case <-c.closedCh:
					if !coalesceTimer.Stop() {
						select {
						case <-coalesceTimer.C:
						default:
						}
					}
					return nil, 0, 0, false
				}
				continue
			}

			c.sendChunks[c.sendChunksHead] = clientSendChunk{}
			c.sendChunksHead++
			if c.sendChunksHead == len(c.sendChunks) {
				c.sendChunks = c.sendChunks[:0]
				c.sendChunksHead = 0
			}
			c.sendLen -= front.len
			notifySpace := c.spaceWaiters > 0
			seq := c.nextSeq
			c.nextSeq++
			c.mu.Unlock()

			if notifySpace {
				select {
				case c.spaceNotify <- struct{}{}:
				default:
				}
			}
			return front.bufp, front.len, seq, true
		}
		c.mu.Unlock()

		select {
		case <-c.sendNotify:
		case <-c.closedCh:
			return nil, 0, 0, false
		}
	}
}

func (c *SplitH2ClientConn) sendChunk(seq uint64, bufp *[]byte, n int) error {
	data := (*bufp)[:n]
	hdr := make(http.Header, 14)
	ApplyBrowserHeaders(hdr, c.fingerprint)
	hdr["Content-Type"] = hdrOctetStream
	hdr["Cache-Control"] = hdrCacheNoStore
	hdr[HeaderSessionID] = c.sidHdr
	var seqBuf [20]byte
	hdr[HeaderStreamSeq] = []string{string(strconv.AppendUint(seqBuf[:0], seq, 10))}

	req := (&http.Request{
		Method:        http.MethodPost,
		URL:           c.endpoint,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(n),
		Host:          c.host,
	}).WithContext(c.reqCtx)
	SignRequestWithRawNonce(req, c.key, time.Now(), c.nextRawNonce())

	resp, err := c.roundTripper(req)
	if err != nil {
		return fmt.Errorf("fronting: split h2 uplink seq=%d: %w", seq, err)
	}
	if resp.ContentLength != 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	}
	_ = resp.Body.Close()

	// Recycle upload chunk buffer only after a completed round-trip so http2 cannot
	// read from it concurrently after returning an error.
	recycleClientSplitChunk(bufp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fronting: split h2 uplink seq=%d status %s", seq, resp.Status)
	}
	if resp.ProtoMajor < 2 {
		return fmt.Errorf("fronting: split h2 uplink seq=%d expected HTTP/2, got %s", seq, resp.Proto)
	}
	return nil
}

func (c *SplitH2ClientConn) abort(err error) {
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
		var unsent []clientSendChunk
		if c.sendChunksHead < len(c.sendChunks) {
			unsent = c.sendChunks[c.sendChunksHead:]
		}
		c.sendChunks = nil
		c.sendChunksHead = 0
		c.sendLen = 0
		reader := c.reader
		cancel := c.reqCancel
		onClose := c.onClose
		c.mu.Unlock()

		for _, ch := range unsent {
			recycleClientSplitChunk(ch.bufp)
		}

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
		close(c.closedCh)
	})
}

func (c *SplitH2ClientConn) Close() error {
	c.abort(net.ErrClosed)
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
		c.abort(os.ErrDeadlineExceeded)
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

func (c *SplitH2ClientConn) onReadDeadline() {
	c.mu.Lock()
	if c.closed || !c.readDeadlineActive || time.Now().Before(c.readDeadlineAt) {
		c.mu.Unlock()
		return
	}
	c.readDeadlineActive = false
	c.readExpired.Store(true)
	c.mu.Unlock()
	c.abort(os.ErrDeadlineExceeded)
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
		c.abort(os.ErrDeadlineExceeded)
		return nil
	}
	c.writeExpired.Store(false)
	c.writeDeadlineAt = t
	c.writeDeadlineActive = true
	// Avoid resetting the runtime timer on every frame when an already-armed
	// timer will fire no later than t; onWriteDeadline re-arms if needed.
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

func (c *SplitH2ClientConn) onWriteDeadline() {
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
	c.abort(os.ErrDeadlineExceeded)
}
