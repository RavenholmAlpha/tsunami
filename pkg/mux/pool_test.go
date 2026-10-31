package mux

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type testReadWriteCloser struct {
	bytes.Buffer
}

func (c *testReadWriteCloser) Close() error {
	return nil
}

// waitFor polls cond until it reports true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf(format, args...)
	}
}

// newPipeSession starts a client-side protocol.Session on one end of a net.Pipe
// and a peer goroutine on the other end that decodes frames. The peer answers
// CmdSYN with CmdSYNACK and, when answerHeartbeats is set, answers
// CmdHeartRequest with CmdHeartResponse. heartbeatSeen, when non-nil, counts
// the heartbeat requests observed by the peer.
//
// Cleanup closes the session and waits for both goroutines to stop so the test
// does not leak background work.
func newPipeSession(t *testing.T, seq uint64, answerHeartbeats bool, heartbeatSeen *atomic.Int64) *protocol.Session {
	t.Helper()

	clientConn, peerConn := net.Pipe()
	session := protocol.NewSession(clientConn, seq)

	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		defer peerConn.Close()
		for {
			frame, err := protocol.DecodeFrame(peerConn)
			if err != nil {
				return
			}
			var reply *protocol.Frame
			switch frame.Command {
			case protocol.CmdSYN:
				reply = protocol.NewFrame(protocol.CmdSYNACK, frame.StreamID, nil)
			case protocol.CmdHeartRequest:
				if heartbeatSeen != nil {
					heartbeatSeen.Add(1)
				}
				if answerHeartbeats {
					reply = protocol.NewFrame(protocol.CmdHeartResponse, 0, nil)
				}
			}
			if reply != nil {
				if err := protocol.EncodeFrame(peerConn, reply); err != nil {
					return
				}
			}
		}
	}()

	eventLoopDone := make(chan struct{})
	go func() {
		defer close(eventLoopDone)
		_ = session.RunEventLoop()
	}()

	t.Cleanup(func() {
		_ = session.Close()
		_ = peerConn.Close()
		select {
		case <-peerDone:
		case <-time.After(2 * time.Second):
			t.Logf("session %d: peer goroutine did not exit", session.Seq())
		}
		select {
		case <-eventLoopDone:
		case <-time.After(2 * time.Second):
			t.Logf("session %d: event loop did not exit", session.Seq())
		}
	})

	return session
}

// newPipePool creates a pool whose dial function returns net.Pipe-backed
// sessions. It returns the pool and the number of dials performed.
func newPipePool(t *testing.T, cfg PoolConfig, answerHeartbeats bool, heartbeatSeen *atomic.Int64) (*Pool, *atomic.Int64) {
	t.Helper()

	var dials atomic.Int64
	pool := NewPool(cfg, func() (*protocol.Session, error) {
		seq := uint64(dials.Add(1))
		return newPipeSession(t, seq, answerHeartbeats, heartbeatSeen), nil
	})
	t.Cleanup(pool.Close)
	return pool, &dials
}

func TestGetOrCreateSessionConcurrentFirstDial(t *testing.T) {
	var dials atomic.Int32
	pool := NewPool(DefaultPoolConfig(), func() (*protocol.Session, error) {
		seq := uint64(dials.Add(1))
		time.Sleep(20 * time.Millisecond)
		return protocol.NewSession(&testReadWriteCloser{}, seq), nil
	})
	defer pool.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.GetOrCreateSession()
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrCreateSession returned error: %v", err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1", got)
	}
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count = %d, want 1", got)
	}
}

func TestDefaultPoolConfigJitteredLifecycle(t *testing.T) {
	cfg := DefaultPoolConfig()

	if cfg.MinIdleSession != 0 {
		t.Errorf("MinIdleSession = %d, want 0 (the pool must be able to drain to zero)", cfg.MinIdleSession)
	}
	if cfg.IdleCheckInterval != 30*time.Second {
		t.Errorf("IdleCheckInterval = %v, want 30s", cfg.IdleCheckInterval)
	}
	if cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("IdleTimeout = %v, want 5m", cfg.IdleTimeout)
	}
	if cfg.HeartbeatMin != 25*time.Second {
		t.Errorf("HeartbeatMin = %v, want 25s", cfg.HeartbeatMin)
	}
	if cfg.HeartbeatMax != 55*time.Second {
		t.Errorf("HeartbeatMax = %v, want 55s", cfg.HeartbeatMax)
	}
	if cfg.HeartbeatTimeout != 90*time.Second {
		t.Errorf("HeartbeatTimeout = %v, want 90s", cfg.HeartbeatTimeout)
	}
	if cfg.HeartbeatMin >= cfg.HeartbeatMax {
		t.Errorf("HeartbeatMin (%v) must be smaller than HeartbeatMax (%v)", cfg.HeartbeatMin, cfg.HeartbeatMax)
	}
	if cfg.HeartbeatTimeout <= cfg.HeartbeatMax {
		t.Errorf("HeartbeatTimeout (%v) must exceed HeartbeatMax (%v) so a jittered ping can be answered",
			cfg.HeartbeatTimeout, cfg.HeartbeatMax)
	}
}

func TestNormalizeConfigFillsDefaults(t *testing.T) {
	def := DefaultPoolConfig()

	got := normalizeConfig(PoolConfig{
		MinIdleSession: 2,
		HeartbeatMin:   10 * time.Millisecond,
	})
	if got.MinIdleSession != 2 {
		t.Errorf("MinIdleSession = %d, want 2 (explicit value must be preserved)", got.MinIdleSession)
	}
	if got.HeartbeatMin != 10*time.Millisecond {
		t.Errorf("HeartbeatMin = %v, want 10ms (explicit value must be preserved)", got.HeartbeatMin)
	}
	if got.HeartbeatMax != def.HeartbeatMax {
		t.Errorf("HeartbeatMax = %v, want default %v", got.HeartbeatMax, def.HeartbeatMax)
	}
	if got.HeartbeatTimeout != def.HeartbeatTimeout {
		t.Errorf("HeartbeatTimeout = %v, want default %v", got.HeartbeatTimeout, def.HeartbeatTimeout)
	}
	if got.IdleCheckInterval != def.IdleCheckInterval {
		t.Errorf("IdleCheckInterval = %v, want default %v", got.IdleCheckInterval, def.IdleCheckInterval)
	}
	if got.IdleTimeout != def.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want default %v", got.IdleTimeout, def.IdleTimeout)
	}

	if zero := normalizeConfig(PoolConfig{}); zero != def {
		t.Errorf("normalizeConfig(PoolConfig{}) = %+v, want %+v", zero, def)
	}

	if neg := normalizeConfig(PoolConfig{MinIdleSession: -5}); neg.MinIdleSession != 0 {
		t.Errorf("normalizeConfig with MinIdleSession=-5 kept %d, want 0", neg.MinIdleSession)
	}

	// A max below min is clamped so the randomized range stays valid.
	clamped := normalizeConfig(PoolConfig{HeartbeatMin: time.Minute, HeartbeatMax: time.Second})
	if clamped.HeartbeatMax != time.Minute {
		t.Errorf("HeartbeatMax = %v, want heartbeat min %v", clamped.HeartbeatMax, time.Minute)
	}
}

func TestHeartbeatDecision(t *testing.T) {
	cfg := normalizeConfig(PoolConfig{
		IdleCheckInterval: time.Second,
		IdleTimeout:       time.Minute,
		HeartbeatMin:      100 * time.Millisecond,
		HeartbeatMax:      200 * time.Millisecond,
		HeartbeatTimeout:  time.Second,
	})
	// Deterministic jitter: always pick the maximum delay.
	randMax := func(min, max time.Duration) time.Duration { return max }
	now := time.Now()

	t.Run("first observation arms schedule from activity", func(t *testing.T) {
		lastActive := now.Add(-10 * time.Millisecond)
		action, st := heartbeatDecision(now, lastActive, now, heartbeatState{}, cfg, randMax)
		if action != heartbeatIdle {
			t.Fatalf("action = %v, want heartbeatIdle", action)
		}
		want := lastActive.Add(cfg.HeartbeatMax)
		if !st.nextPing.Equal(want) {
			t.Errorf("nextPing = %v, want %v", st.nextPing, want)
		}
		if !st.awaitingSince.IsZero() {
			t.Errorf("awaitingSince = %v, want zero", st.awaitingSince)
		}
	})

	t.Run("sends when due and no recent traffic", func(t *testing.T) {
		lastActive := now.Add(-cfg.HeartbeatMin - time.Millisecond)
		st := heartbeatState{nextPing: now.Add(-time.Millisecond)}
		action, next := heartbeatDecision(now, lastActive, now, st, cfg, randMax)
		if action != heartbeatSend {
			t.Fatalf("action = %v, want heartbeatSend", action)
		}
		if !next.awaitingSince.Equal(now) {
			t.Errorf("awaitingSince = %v, want %v", next.awaitingSince, now)
		}
		if want := now.Add(cfg.HeartbeatMax); !next.nextPing.Equal(want) {
			t.Errorf("nextPing = %v, want %v", next.nextPing, want)
		}
	})

	t.Run("recent traffic postpones the ping", func(t *testing.T) {
		lastActive := now.Add(-20 * time.Millisecond)
		st := heartbeatState{nextPing: now.Add(-time.Millisecond)}
		action, next := heartbeatDecision(now, lastActive, now, st, cfg, randMax)
		if action != heartbeatIdle {
			t.Fatalf("action = %v, want heartbeatIdle (ping within HeartbeatMin of traffic)", action)
		}
		want := lastActive.Add(cfg.HeartbeatMax)
		if !next.nextPing.Equal(want) {
			t.Errorf("nextPing = %v, want %v", next.nextPing, want)
		}
		if !next.nextPing.After(now) {
			t.Errorf("nextPing = %v, want it strictly after now (%v)", next.nextPing, now)
		}
	})

	t.Run("pong clears the outstanding heartbeat", func(t *testing.T) {
		lastActive := now.Add(-10 * time.Millisecond)
		st := heartbeatState{
			nextPing:      now.Add(-50 * time.Millisecond),
			awaitingSince: now.Add(-50 * time.Millisecond),
		}
		lastPong := now.Add(-40 * time.Millisecond) // after awaitingSince
		action, next := heartbeatDecision(now, lastActive, lastPong, st, cfg, randMax)
		if action != heartbeatIdle {
			t.Fatalf("action = %v, want heartbeatIdle", action)
		}
		if !next.awaitingSince.IsZero() {
			t.Errorf("awaitingSince = %v, want zero after pong", next.awaitingSince)
		}
		if want := lastActive.Add(cfg.HeartbeatMax); !next.nextPing.Equal(want) {
			t.Errorf("nextPing = %v, want %v", next.nextPing, want)
		}
	})

	t.Run("outstanding heartbeat within timeout waits", func(t *testing.T) {
		awaiting := now.Add(-500 * time.Millisecond)
		lastPong := now.Add(-2 * time.Second) // before awaiting: no pong seen yet
		st := heartbeatState{nextPing: awaiting, awaitingSince: awaiting}
		action, next := heartbeatDecision(now, now.Add(-time.Minute), lastPong, st, cfg, randMax)
		if action != heartbeatIdle {
			t.Fatalf("action = %v, want heartbeatIdle (still inside HeartbeatTimeout)", action)
		}
		if !next.awaitingSince.Equal(awaiting) {
			t.Errorf("awaitingSince = %v, want it unchanged at %v", next.awaitingSince, awaiting)
		}
	})

	t.Run("missing pong past timeout closes the session", func(t *testing.T) {
		awaiting := now.Add(-2 * time.Second)
		lastPong := now.Add(-3 * time.Second)
		st := heartbeatState{nextPing: awaiting, awaitingSince: awaiting}
		action, _ := heartbeatDecision(now, now.Add(-time.Minute), lastPong, st, cfg, randMax)
		if action != heartbeatClose {
			t.Fatalf("action = %v, want heartbeatClose", action)
		}
	})
}

func TestHeartbeatTickInterval(t *testing.T) {
	cases := []struct {
		name string
		cfg  PoolConfig
		want time.Duration
	}{
		{"default capped at 5s", DefaultPoolConfig(), 5 * time.Second},
		{"quarter of small heartbeat max", PoolConfig{HeartbeatMax: 60 * time.Millisecond}, 15 * time.Millisecond},
		{"quarter of medium heartbeat max", PoolConfig{HeartbeatMax: 8 * time.Second}, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := heartbeatTickInterval(tc.cfg); got != tc.want {
				t.Fatalf("heartbeatTickInterval(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

func TestIdleCloseDeadlineJitter(t *testing.T) {
	base := 100 * time.Millisecond

	low := idleCloseDeadline(base, func() float64 { return 0 })
	if low < 69*time.Millisecond || low > 71*time.Millisecond {
		t.Errorf("idleCloseDeadline(f=0) = %v, want about %v", low, 70*time.Millisecond)
	}

	high := idleCloseDeadline(base, func() float64 { return 0.999999 })
	if high < 129*time.Millisecond || high > 131*time.Millisecond {
		t.Errorf("idleCloseDeadline(f~1) = %v, want about %v", high, 130*time.Millisecond)
	}

	mid := idleCloseDeadline(base, func() float64 { return 0.5 })
	if mid < 99*time.Millisecond || mid > 101*time.Millisecond {
		t.Errorf("idleCloseDeadline(f=0.5) = %v, want about %v", mid, base)
	}

	if low == high {
		t.Errorf("jitter produced identical deadlines (%v) at both ends of the range", low)
	}
}

func TestIdleSessionClosedAfterRandomizedTimeout(t *testing.T) {
	pool, dials := newPipePool(t, PoolConfig{
		MinIdleSession:    0,
		IdleCheckInterval: 20 * time.Millisecond,
		IdleTimeout:       80 * time.Millisecond,
	}, true, nil)

	first, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count = %d, want 1", got)
	}

	waitFor(t, 2*time.Second, func() bool { return pool.SessionCount() == 0 && first.IsClosed() },
		"idle session was not closed within 2s with MinIdleSession=0")

	second, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("GetOrCreateSession after idle close: %v", err)
	}
	if second == first || second.Seq() == first.Seq() {
		t.Fatalf("GetOrCreateSession returned the closed idle session (seq=%d)", second.Seq())
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dial count = %d, want 2 (a new session must be dialed)", got)
	}
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count = %d, want 1 after re-dial", got)
	}
}

func TestMinIdleSessionKeepsIdleSessions(t *testing.T) {
	pool, dials := newPipePool(t, PoolConfig{
		MinIdleSession:    1,
		IdleCheckInterval: 20 * time.Millisecond,
		IdleTimeout:       60 * time.Millisecond,
	}, true, nil)

	for i := 0; i < 3; i++ {
		if _, err := pool.CreateNewSession(); err != nil {
			t.Fatalf("CreateNewSession %d: %v", i, err)
		}
	}
	if got := dials.Load(); got != 3 {
		t.Fatalf("dial count = %d, want 3", got)
	}

	waitFor(t, 2*time.Second, func() bool { return pool.SessionCount() == 1 },
		"pool did not drain down to MinIdleSession=1")

	// The kept session must stay put: MinIdleSession is a floor, not a target.
	time.Sleep(150 * time.Millisecond)
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count after settling = %d, want 1", got)
	}
}

func TestIdleCleanupSkipsSessionsWithActiveStreams(t *testing.T) {
	pool, _ := newPipePool(t, PoolConfig{
		MinIdleSession:    0,
		IdleCheckInterval: 20 * time.Millisecond,
		IdleTimeout:       50 * time.Millisecond,
	}, true, nil)

	session, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	stream, err := session.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	// Idle cleanup must not retire a session that is carrying a stream.
	time.Sleep(200 * time.Millisecond)
	if session.IsClosed() {
		t.Fatalf("session with an active stream was closed by idle cleanup")
	}
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count = %d, want 1 while the stream is active", got)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("stream.Close: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return pool.SessionCount() == 0 },
		"session was not closed after its last stream went away")
}

func TestHeartbeatPongKeepsSessionAlive(t *testing.T) {
	var heartbeats atomic.Int64
	pool, _ := newPipePool(t, PoolConfig{
		HeartbeatMin:     30 * time.Millisecond,
		HeartbeatMax:     60 * time.Millisecond,
		HeartbeatTimeout: 250 * time.Millisecond,
	}, true, &heartbeats)

	session, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	time.Sleep(600 * time.Millisecond)

	if got := heartbeats.Load(); got < 2 {
		t.Fatalf("peer observed %d heartbeat requests in 600ms, want at least 2", got)
	}
	if session.IsClosed() {
		t.Fatalf("session was closed although the peer answered every heartbeat")
	}
	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count = %d, want 1 while heartbeats are answered", got)
	}
}

func TestHeartbeatTimeoutClosesUnresponsiveSession(t *testing.T) {
	var heartbeats atomic.Int64
	// The peer decodes frames (so writes do not block) but never answers.
	pool, _ := newPipePool(t, PoolConfig{
		HeartbeatMin:     30 * time.Millisecond,
		HeartbeatMax:     60 * time.Millisecond,
		HeartbeatTimeout: 100 * time.Millisecond,
	}, false, &heartbeats)

	session, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool { return pool.SessionCount() == 0 && session.IsClosed() },
		"unresponsive session was not closed after HeartbeatTimeout")
	if got := heartbeats.Load(); got < 1 {
		t.Fatalf("peer observed %d heartbeat requests, want at least 1", got)
	}
}

func TestHeartbeatStatePrunedWhenSessionRemoved(t *testing.T) {
	pool, _ := newPipePool(t, PoolConfig{
		HeartbeatMin: time.Hour,
		HeartbeatMax: 2 * time.Hour,
	}, false, nil)

	session, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	now := time.Now()
	pool.mu.Lock()
	pool.heartbeatState[session] = heartbeatState{nextPing: now.Add(time.Hour)}
	pool.mu.Unlock()

	// A still-live session keeps its bookkeeping.
	pool.runHeartbeat(now)
	pool.mu.Lock()
	_, hasState := pool.heartbeatState[session]
	pool.mu.Unlock()
	if !hasState {
		t.Fatalf("heartbeat state disappeared for a live session")
	}

	// A session that left the pool must lose its bookkeeping.
	_ = session.Close()
	pool.runHeartbeat(time.Now().Add(time.Second))
	pool.mu.Lock()
	_, hasState = pool.heartbeatState[session]
	sessionCount := len(pool.sessions)
	pool.mu.Unlock()
	if hasState {
		t.Errorf("heartbeat state was not pruned for a closed session")
	}
	if sessionCount != 0 {
		t.Errorf("pool kept %d closed session(s), want 0", sessionCount)
	}
}

func TestCloseIdleSessionsKeepsMinKeep(t *testing.T) {
	pool, _ := newPipePool(t, PoolConfig{MinIdleSession: 0}, true, nil)

	first, err := pool.CreateNewSession()
	if err != nil {
		t.Fatalf("CreateNewSession first: %v", err)
	}
	second, err := pool.CreateNewSession()
	if err != nil {
		t.Fatalf("CreateNewSession second: %v", err)
	}

	oldIdle := time.Now().Add(-time.Hour)
	first.SetIdleSince(oldIdle)
	second.SetIdleSince(oldIdle)

	pool.mu.Lock()
	pool.heartbeatState[first] = heartbeatState{nextPing: time.Now().Add(time.Hour)}
	pool.heartbeatState[second] = heartbeatState{nextPing: time.Now().Add(time.Hour)}
	pool.mu.Unlock()

	pool.CloseIdleSessions(time.Minute, 1)

	if got := pool.SessionCount(); got != 1 {
		t.Fatalf("session count = %d, want 1 (minKeep=1)", got)
	}
	if !first.IsClosed() {
		t.Errorf("most-idle session was not closed")
	}
	if second.IsClosed() {
		t.Errorf("session retained for minKeep=1 was closed")
	}
	pool.mu.Lock()
	_, hasFirst := pool.heartbeatState[first]
	_, hasSecond := pool.heartbeatState[second]
	pool.mu.Unlock()
	if hasFirst {
		t.Errorf("heartbeat state was not removed for the closed session")
	}
	if !hasSecond {
		t.Errorf("heartbeat state was removed for the retained session")
	}
}

func TestPoolCloseClosesSessionsAndClearsState(t *testing.T) {
	pool, _ := newPipePool(t, DefaultPoolConfig(), true, nil)

	session, err := pool.GetOrCreateSession()
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	pool.mu.Lock()
	pool.heartbeatState[session] = heartbeatState{nextPing: time.Now().Add(time.Hour)}
	pool.mu.Unlock()

	pool.Close()

	if !session.IsClosed() {
		t.Fatalf("session was not closed by Pool.Close")
	}
	if got := pool.SessionCount(); got != 0 {
		t.Fatalf("session count after Close = %d, want 0", got)
	}
	pool.mu.Lock()
	stateLen := len(pool.heartbeatState)
	sessionLen := len(pool.sessions)
	pool.mu.Unlock()
	if stateLen != 0 {
		t.Errorf("heartbeat state after Close has %d entries, want 0", stateLen)
	}
	if sessionLen != 0 {
		t.Errorf("pool holds %d sessions after Close, want 0", sessionLen)
	}

	// Close is idempotent and must not panic or block.
	pool.Close()
}
