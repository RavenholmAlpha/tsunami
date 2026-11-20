package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

func TestApplyPaddingSchemeUpdateUpdatesClientAndWriter(t *testing.T) {
	c := &Client{paddingScheme: padding.DefaultScheme()}
	var out bytes.Buffer
	writer := padding.NewWriter(&out, c.currentPaddingScheme())

	schemeText := "stop=2\nctl=~50-50\n0=~60-60\n1=~80-80"
	expected, err := padding.Parse(schemeText)
	if err != nil {
		t.Fatalf("parse expected scheme: %v", err)
	}

	if err := c.applyPaddingSchemeUpdate([]byte(schemeText), writer); err != nil {
		t.Fatalf("apply padding update: %v", err)
	}
	if got := c.currentPaddingScheme().MD5(); got != expected.MD5() {
		t.Fatalf("client scheme md5 = %s, want %s", got, expected.MD5())
	}

	// Control frames use the ctl rule (~50).
	before := out.Len()
	if err := writer.WriteFramesWithPadding([]*protocol.Frame{
		protocol.NewFrame(protocol.CmdFIN, 1, nil),
	}); err != nil {
		t.Fatalf("write padded control frame: %v", err)
	}
	if got := out.Len() - before; got != 50 {
		t.Fatalf("padded control write length = %d, want 50", got)
	}

	// The per-stream epoch starts at rule 0 (~60), then rule 1 (~80).
	before = out.Len()
	if err := writer.WriteFramesWithPadding([]*protocol.Frame{
		protocol.NewFrame(protocol.CmdPSH, 3, make([]byte, 10)),
	}); err != nil {
		t.Fatalf("write padded stream frame: %v", err)
	}
	if got := out.Len() - before; got != 60 {
		t.Fatalf("first stream write length = %d, want 60", got)
	}

	before = out.Len()
	if err := writer.WriteFramesWithPadding([]*protocol.Frame{
		protocol.NewFrame(protocol.CmdPSH, 3, make([]byte, 10)),
	}); err != nil {
		t.Fatalf("write second padded stream frame: %v", err)
	}
	if got := out.Len() - before; got != 80 {
		t.Fatalf("second stream write length = %d, want 80", got)
	}
}

func TestApplyPaddingSchemeUpdateRejectsInvalidScheme(t *testing.T) {
	original := padding.DefaultScheme()
	c := &Client{paddingScheme: original}

	if err := c.applyPaddingSchemeUpdate([]byte("stop=bad"), nil); err == nil {
		t.Fatal("expected invalid scheme error")
	}
	if got := c.currentPaddingScheme().MD5(); got != original.MD5() {
		t.Fatalf("client scheme changed after invalid update: got %s, want %s", got, original.MD5())
	}
}

func TestAuthPaddingLengthRange(t *testing.T) {
	// Auth padding is decoupled from the padding scheme rules and always stays
	// inside the 24..88 byte range.
	seen := make(map[int]struct{})
	for i := 0; i < 2000; i++ {
		n := authPaddingLen()
		if n < authPaddingMin || n > authPaddingMax {
			t.Fatalf("auth padding length = %d, want %d..%d", n, authPaddingMin, authPaddingMax)
		}
		seen[n] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("auth padding length is not randomized: only saw %v", seen)
	}
}

func TestAuthReadErrorClassifiesTimeoutAndEOF(t *testing.T) {
	timeoutAuthErr := newAuthReadError("example.com:443", fmt.Errorf("read frame: %w", timeoutError{}))
	if timeoutAuthErr.Reason != AuthFailureTimeout {
		t.Fatalf("timeout reason = %s, want %s", timeoutAuthErr.Reason, AuthFailureTimeout)
	}
	if !errors.Is(timeoutAuthErr, protocol.ErrAuthFailed) {
		t.Fatal("timeout auth error should match protocol.ErrAuthFailed")
	}
	if !strings.Contains(timeoutAuthErr.Error(), string(AuthFailureTimeout)) {
		t.Fatalf("timeout auth error missing reason: %v", timeoutAuthErr)
	}

	closedAuthErr := newAuthReadError("example.com:443", io.EOF)
	if closedAuthErr.Reason != AuthFailureConnectionClosed {
		t.Fatalf("EOF reason = %s, want %s", closedAuthErr.Reason, AuthFailureConnectionClosed)
	}
}

func TestUnexpectedAuthFrameErrorIncludesFrameMetadata(t *testing.T) {
	frame := protocol.NewFrame(protocol.CmdPSH, 7, []byte("fallback"))
	authErr := newUnexpectedAuthFrameError("example.com:443", frame)

	if authErr.Reason != AuthFailureUnexpectedFrame {
		t.Fatalf("reason = %s, want %s", authErr.Reason, AuthFailureUnexpectedFrame)
	}
	if authErr.Command != protocol.CmdPSH || authErr.StreamID != 7 || authErr.DataLen != len("fallback") {
		t.Fatalf("unexpected frame metadata: command=%d stream=%d data_len=%d", authErr.Command, authErr.StreamID, authErr.DataLen)
	}
	if !errors.Is(authErr, protocol.ErrAuthFailed) {
		t.Fatal("unexpected frame auth error should match protocol.ErrAuthFailed")
	}
}

func TestOpenStreamRetriesAndEvictsDeadSession(t *testing.T) {
	var dials atomic.Int32

	pool := mux.NewPool(mux.DefaultPoolConfig(), func() (*protocol.Session, error) {
		n := dials.Add(1)
		cConn, pConn := net.Pipe()
		sess := protocol.NewSession(cConn, uint64(n))

		if n == 1 {
			// First session: peer closes immediately on read so OpenStream fails.
			go func() {
				buf := make([]byte, 64)
				_, _ = pConn.Read(buf)
				_ = pConn.Close()
			}()
		} else {
			// Second session: healthy peer responding with SYNACK.
			go func() {
				defer pConn.Close()
				for {
					f, err := protocol.DecodeFrame(pConn)
					if err != nil {
						return
					}
					if f.Command == protocol.CmdSYN {
						_ = protocol.EncodeFrame(pConn, protocol.NewFrame(protocol.CmdSYNACK, f.StreamID, nil))
					}
				}
			}()
		}
		go func() { _ = sess.RunEventLoop() }()
		return sess, nil
	})
	defer pool.Close()

	c := &Client{
		config:        DefaultConfig(),
		pool:          pool,
		surgeCtrl:     surge.NewController(surge.Config{Mode: surge.ModeNone}, pool),
		paddingScheme: padding.DefaultScheme(),
	}
	defer c.Close()

	// Seed the dead first session into the pool.
	deadSess, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("seed dead session: %v", err)
	}

	// OpenStream should fail on deadSess, evict it, redial session 2, and succeed!
	stream, err := c.OpenStream("example.com:443")
	if err != nil {
		t.Fatalf("OpenStream should have retried on healthy session, got error: %v", err)
	}
	defer stream.Close()

	if !deadSess.IsClosed() {
		t.Fatal("dead session was not evicted and closed")
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dial count = %d, want 2", got)
	}
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("SessionCount = %d, want 1", got)
	}

	// Drain target address bytes written on stream cleanup cleanly.
	_ = time.Millisecond
}
