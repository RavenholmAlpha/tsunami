package surge

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

func newTestPool() *mux.Pool {
	var seq atomic.Uint64
	return mux.NewPool(mux.DefaultPoolConfig(), func() (*protocol.Session, error) {
		return newTestSession(seq.Add(1)), nil
	})
}

func newTestSession(seq uint64) *protocol.Session {
	clientConn, peerConn := net.Pipe()
	session := protocol.NewSession(clientConn, seq)

	go func() {
		defer peerConn.Close()
		for {
			frame, err := protocol.DecodeFrame(peerConn)
			if err != nil {
				return
			}

			switch frame.Command {
			case protocol.CmdSYN:
				err = protocol.EncodeFrame(peerConn, protocol.NewFrame(protocol.CmdSYNACK, frame.StreamID, nil))
			case protocol.CmdHeartRequest:
				err = protocol.EncodeFrame(peerConn, protocol.NewFrame(protocol.CmdHeartResponse, 0, nil))
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		_ = session.RunEventLoop()
	}()
	return session
}

func TestNewControllerAppliesDefaults(t *testing.T) {
	pool := newTestPool()
	defer pool.Close()

	controller := NewController(Config{Mode: Mode("invalid")}, pool)
	defer controller.Stop()

	if controller.config.Mode != ModeAuto {
		t.Fatalf("mode = %q, want %q", controller.config.Mode, ModeAuto)
	}
	if controller.config.Threshold != DefaultConfig().Threshold {
		t.Fatalf("threshold = %d, want %d", controller.config.Threshold, DefaultConfig().Threshold)
	}
	if controller.config.MaxConnections != DefaultConfig().MaxConnections {
		t.Fatalf("max connections = %d, want %d", controller.config.MaxConnections, DefaultConfig().MaxConnections)
	}
	if controller.config.IdleDowngradeTime != DefaultConfig().IdleDowngradeTime {
		t.Fatalf("idle downgrade = %s, want %s", controller.config.IdleDowngradeTime, DefaultConfig().IdleDowngradeTime)
	}
}

func TestGetSessionExpandsImmediatelyWhenNextStreamCrossesThreshold(t *testing.T) {
	pool := newTestPool()
	defer pool.Close()

	first, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := first.OpenStream(); err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
	}

	controller := NewController(Config{
		Mode:              ModeAuto,
		Threshold:         2,
		MaxConnections:    3,
		IdleDowngradeTime: time.Second,
	}, pool)
	defer controller.Stop()

	selected, err := controller.GetSession()
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if controller.CurrentLayer() != 2 {
		t.Fatalf("layer = %d, want 2", controller.CurrentLayer())
	}
	if pool.SessionCount() < 2 {
		t.Fatalf("session count = %d, want at least 2", pool.SessionCount())
	}
	if selected.Seq() == first.Seq() {
		t.Fatalf("selected existing overloaded session seq=%d, want a new session", selected.Seq())
	}
}

func TestApplyServerSettings(t *testing.T) {
	pool := newTestPool()
	defer pool.Close()

	c := NewController(Config{
		Mode:           ModeAuto,
		Threshold:      8,
		MaxConnections: 4,
	}, pool)
	defer c.Stop()

	c.ApplyServerSettings(ModeAuto, 2, 5)
	c.mu.Lock()
	if c.config.MaxConnections != 2 || c.config.Threshold != 5 {
		t.Fatalf("ApplyServerSettings got maxConn=%d threshold=%d, want 2, 5", c.config.MaxConnections, c.config.Threshold)
	}
	c.mu.Unlock()

	c.ApplyServerSettings(ModeNone, 4, 8)
	if c.CurrentLayer() != 1 {
		t.Fatalf("CurrentLayer = %d, want 1 after ModeNone", c.CurrentLayer())
	}
	c.mu.Lock()
	if c.config.Mode != ModeNone {
		t.Fatalf("Mode = %q, want %q", c.config.Mode, ModeNone)
	}
	c.mu.Unlock()
}

func TestExpandPoolDialsInParallel(t *testing.T) {
	var seq atomic.Uint64
	pool := mux.NewPool(mux.DefaultPoolConfig(), func() (*protocol.Session, error) {
		time.Sleep(60 * time.Millisecond)
		return newTestSession(seq.Add(1)), nil
	})
	defer pool.Close()

	c := NewController(Config{
		Mode:           ModeAuto,
		Threshold:      2,
		MaxConnections: 4,
	}, pool)
	defer c.Stop()

	start := time.Now()
	c.expandPool(4)
	elapsed := time.Since(start)

	if got := pool.SessionCount(); got != 4 {
		t.Fatalf("SessionCount = %d, want 4", got)
	}
	// Serialized dialing of 4 x 60ms would take >= 240ms; parallel dialing finishes in ~60-120ms.
	if elapsed >= 180*time.Millisecond {
		t.Fatalf("expandPool(4) took %v, expected parallel dials (< 180ms)", elapsed)
	}
}
