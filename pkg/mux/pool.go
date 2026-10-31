// Package mux implements the TSUNAMI session pool and connection multiplexing.
package mux

import (
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// PoolConfig configures the session pool behavior.
//
// The lifecycle knobs exist to keep the client from looking like a synthetic
// beacon: a client that always keeps one TLS connection open, or that emits an
// application heartbeat on a fixed period, is easy to distinguish from real
// browsing traffic. Wherever the phase of an event would otherwise be
// predictable it is randomized, and the pool is allowed to drain to zero
// sessions so an idle client has no tunnel at all.
type PoolConfig struct {
	// MinIdleSession is the minimum number of idle sessions to keep alive.
	// Default 0: when the user is idle the pool drains completely, so no TLS
	// connection stays open 24/7 as a synthetic-traffic tell.
	MinIdleSession int
	// IdleCheckInterval is how often to scan for idle sessions. Default 30s.
	IdleCheckInterval time.Duration
	// IdleTimeout is how long a session may stay idle before being closed.
	// The per-session deadline is scaled by a random factor in [0.7, 1.3) on
	// every check, so a fleet of idle sessions does not disappear in lockstep
	// on a fixed schedule. Default 5m.
	IdleTimeout time.Duration
	// HeartbeatMin is the minimum delay before the next application heartbeat,
	// and also the minimum quiet period after any observed traffic. Default 25s.
	HeartbeatMin time.Duration
	// HeartbeatMax is the upper bound of the randomized heartbeat delay.
	// The next ping is scheduled at lastActive + rand[HeartbeatMin, HeartbeatMax]
	// so heartbeats never form a fixed period. Default 55s.
	HeartbeatMax time.Duration
	// HeartbeatTimeout is how long an outstanding heartbeat may stay unanswered
	// before the session is declared dead and closed. Default 90s.
	HeartbeatTimeout time.Duration
}

// DefaultPoolConfig returns sensible defaults.
//
// MinIdleSession is 0 (no permanent idle session) and the heartbeat schedule is
// randomized rather than a fixed 15s period, because both a 24/7 idle TLS
// tunnel and a machine-precise application heartbeat are synthetic-traffic
// fingerprints.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MinIdleSession:    0,
		IdleCheckInterval: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		HeartbeatMin:      25 * time.Second,
		HeartbeatMax:      55 * time.Second,
		HeartbeatTimeout:  90 * time.Second,
	}
}

// normalizeConfig fills zero or invalid fields from DefaultPoolConfig so
// callers constructing a partial PoolConfig (or the zero value) still get sane
// behavior.
func normalizeConfig(cfg PoolConfig) PoolConfig {
	def := DefaultPoolConfig()

	if cfg.MinIdleSession < 0 {
		cfg.MinIdleSession = 0
	}
	if cfg.IdleCheckInterval <= 0 {
		cfg.IdleCheckInterval = def.IdleCheckInterval
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = def.IdleTimeout
	}
	if cfg.HeartbeatMin <= 0 {
		cfg.HeartbeatMin = def.HeartbeatMin
	}
	if cfg.HeartbeatMax <= 0 {
		cfg.HeartbeatMax = def.HeartbeatMax
	}
	if cfg.HeartbeatMax < cfg.HeartbeatMin {
		cfg.HeartbeatMax = cfg.HeartbeatMin
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = def.HeartbeatTimeout
	}

	return cfg
}

// heartbeatAction is the outcome of a keep-alive decision for one session.
type heartbeatAction int

const (
	// heartbeatIdle means no action is needed for this session right now.
	heartbeatIdle heartbeatAction = iota
	// heartbeatSend means a CmdHeartRequest should be sent.
	heartbeatSend
	// heartbeatClose means the session is considered dead and should be closed.
	heartbeatClose
)

// heartbeatState is the pool-side keep-alive bookkeeping for one session.
// It is only accessed while holding Pool.mu.
type heartbeatState struct {
	// nextPing is the earliest time at which a heartbeat may be sent. It is
	// anchored to the session's last activity plus a random delay so a client
	// that is actively using the tunnel never appears to ping on a fixed clock.
	nextPing time.Time
	// awaitingSince is when the outstanding heartbeat was sent. It is the zero
	// value when no pong is outstanding.
	awaitingSince time.Time
}

// Pool manages a pool of reusable Sessions for connection multiplexing.
type Pool struct {
	config     PoolConfig
	sessions   []*protocol.Session
	mu         sync.Mutex
	createMu   sync.Mutex
	seqCounter atomic.Uint64
	stopCh     chan struct{}
	stopped    atomic.Bool

	// heartbeatState holds the keep-alive state for each session; guarded by mu.
	heartbeatState map[*protocol.Session]heartbeatState

	// Jitter sources. They are unexported so tests can substitute deterministic
	// functions; they are not expected to change while the pool is in use.
	randDuration func(min, max time.Duration) time.Duration
	randFloat64  func() float64

	// Dial function to create new TLS connections.
	dialFn func() (*protocol.Session, error)
}

// NewPool creates a new session pool.
func NewPool(config PoolConfig, dialFn func() (*protocol.Session, error)) *Pool {
	p := &Pool{
		config:         normalizeConfig(config),
		dialFn:         dialFn,
		stopCh:         make(chan struct{}),
		heartbeatState: make(map[*protocol.Session]heartbeatState),
		randDuration:   defaultRandDuration,
		randFloat64:    rand.Float64,
	}
	go p.idleChecker()
	go p.heartbeatLoop()
	return p
}

// GetOrCreateSession returns an existing session or creates a new one.
// Strategy: prefer idle sessions (newest first), fall back to any non-closed
// session for multiplexing, create new only if no sessions exist at all.
func (p *Pool) GetOrCreateSession() (*protocol.Session, error) {
	p.mu.Lock()

	var bestIdle *protocol.Session
	var bestAny *protocol.Session

	for _, s := range p.sessions {
		if s.IsClosed() {
			continue
		}
		// Track any non-closed session (prefer newest)
		if bestAny == nil || s.Seq() > bestAny.Seq() {
			bestAny = s
		}
		// Prefer idle sessions
		if s.IsIdle() && (bestIdle == nil || s.Seq() > bestIdle.Seq()) {
			bestIdle = s
		}
	}

	p.mu.Unlock()

	// Prefer idle session for clean reuse
	if bestIdle != nil {
		return bestIdle, nil
	}
	// Fall back to any existing session — this is multiplexing
	if bestAny != nil {
		return bestAny, nil
	}

	// No sessions at all — create the first one
	p.createMu.Lock()
	defer p.createMu.Unlock()

	p.mu.Lock()
	for _, s := range p.sessions {
		if !s.IsClosed() {
			p.mu.Unlock()
			return s, nil
		}
	}
	p.mu.Unlock()

	return p.createSession()
}

// GetLeastLoadedSession returns the session with the fewest active streams.
// Used by Surge bonded mode for stream distribution.
func (p *Pool) GetLeastLoadedSession() (*protocol.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var best *protocol.Session
	bestCount := int(^uint(0) >> 1) // max int

	for _, s := range p.sessions {
		if s.IsClosed() {
			continue
		}
		count := s.ActiveStreamCount()
		if count < bestCount {
			best = s
			bestCount = count
		}
	}

	if best != nil {
		return best, nil
	}

	return nil, fmt.Errorf("tsunami: no available sessions in pool")
}

// createSession dials a new TLS connection and adds it to the pool.
func (p *Pool) createSession() (*protocol.Session, error) {
	if p.stopped.Load() {
		return nil, fmt.Errorf("tsunami: session pool is closed")
	}

	session, err := p.dialFn()
	if err != nil {
		return nil, fmt.Errorf("tsunami: dial new session: %w", err)
	}

	p.mu.Lock()
	if p.stopped.Load() {
		p.mu.Unlock()
		_ = session.Close()
		return nil, fmt.Errorf("tsunami: session pool is closed")
	}
	p.sessions = append(p.sessions, session)
	p.mu.Unlock()

	// Start the idle clock for a session that has not carried a stream yet;
	// protocol.Session only records IdleSince when the last stream closes.
	if !session.IsClosed() && session.IsIdle() && session.IdleSince().IsZero() {
		session.SetIdleSince(time.Now())
	}

	return session, nil
}

// CreateNewSession forces creation of a new TLS connection and adds it to the pool.
// Unlike GetOrCreateSession which reuses existing sessions, this always dials.
// Used by Surge controller for intentional pool expansion.
func (p *Pool) CreateNewSession() (*protocol.Session, error) {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	return p.createSession()
}

// AddSession adds an externally created session to the pool.
func (p *Pool) AddSession(s *protocol.Session) {
	if s == nil {
		return
	}

	p.mu.Lock()
	if p.stopped.Load() {
		p.mu.Unlock()
		_ = s.Close()
		return
	}
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()

	if !s.IsClosed() && s.IsIdle() && s.IdleSince().IsZero() {
		s.SetIdleSince(time.Now())
	}
}

// SessionCount returns the number of active (non-closed) sessions.
func (p *Pool) SessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0
	for _, s := range p.sessions {
		if !s.IsClosed() {
			count++
		}
	}
	return count
}

// ActiveStreamCount returns the total number of active streams across all sessions.
func (p *Pool) ActiveStreamCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	total := 0
	for _, s := range p.sessions {
		if !s.IsClosed() {
			total += s.ActiveStreamCount()
		}
	}
	return total
}

// NextSeq returns the next monotonically increasing session sequence number.
func (p *Pool) NextSeq() uint64 {
	return p.seqCounter.Add(1)
}

// idleChecker periodically checks and closes idle sessions.
func (p *Pool) idleChecker() {
	ticker := time.NewTicker(p.config.IdleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if p.stopped.Load() {
				return
			}
			p.cleanupIdle()
		}
	}
}

// idleCloseDeadline returns IdleTimeout scaled by a random factor in [0.7, 1.3).
// The jitter prevents idle sessions (and therefore connection teardown) from
// happening on a predictable period, which would itself be a traffic pattern.
func idleCloseDeadline(base time.Duration, randFloat func() float64) time.Duration {
	factor := 0.7 + 0.6*randFloat() // [0.7, 1.3)
	return time.Duration(float64(base) * factor)
}

// cleanupIdle removes sessions that have been idle too long.
//
// A session is closable once it has been idle longer than a randomized
// IdleTimeout, but at least MinIdleSession idle sessions (0 by default) are
// kept. The default of 0 means an idle client eventually has no open TLS
// connection at all instead of a permanent tunnel.
func (p *Pool) cleanupIdle() {
	type idleCandidate struct {
		session *protocol.Session
		idleFor time.Duration
	}

	now := time.Now()

	p.mu.Lock()

	// First pass: drop already-closed sessions and their keep-alive state.
	live := make([]*protocol.Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		if s.IsClosed() {
			delete(p.heartbeatState, s)
			continue
		}
		live = append(live, s)
	}
	p.sessions = live

	idleCount := 0
	candidates := make([]idleCandidate, 0, len(live))

	for _, s := range live {
		if !s.IsIdle() {
			continue
		}
		idleCount++

		idleSince := s.IdleSince()
		if idleSince.IsZero() {
			// Safety net: a session that never carried a stream has no idle
			// timestamp. Start its idle clock now rather than keeping it
			// forever (a permanent idle tunnel is exactly what we avoid).
			s.SetIdleSince(now)
			continue
		}

		idleFor := now.Sub(idleSince)
		// Random deadline per session and per check: no shared cutoff.
		if idleFor > idleCloseDeadline(p.config.IdleTimeout, p.randFloat64) {
			candidates = append(candidates, idleCandidate{session: s, idleFor: idleFor})
		}
	}

	closeable := idleCount - p.config.MinIdleSession
	if closeable <= 0 || len(candidates) == 0 {
		p.mu.Unlock()
		return
	}

	// Close the longest-idle sessions first so the sessions we keep are the
	// most recently used ones.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].idleFor > candidates[j].idleFor
	})

	closing := make(map[*protocol.Session]struct{}, len(candidates))
	for _, c := range candidates {
		if closeable <= 0 {
			break
		}
		closing[c.session] = struct{}{}
		delete(p.heartbeatState, c.session)
		closeable--
	}

	remaining := make([]*protocol.Session, 0, len(live))
	for _, s := range live {
		if _, ok := closing[s]; ok {
			continue
		}
		remaining = append(remaining, s)
	}
	p.sessions = remaining
	p.mu.Unlock()

	// Session.Close can write to the connection, so it must happen outside the
	// pool lock.
	for s := range closing {
		log.Printf("tsunami: closing idle session %d (idle longer than %v)", s.Seq(), p.config.IdleTimeout)
		_ = s.Close()
	}
}

// heartbeatTickInterval returns the interval of the heartbeat scheduler tick.
// It is deliberately shorter than the heartbeat period so jittered deadlines
// are honored with reasonable precision, and capped at 5s.
func heartbeatTickInterval(cfg PoolConfig) time.Duration {
	interval := cfg.HeartbeatMax / 4
	if interval <= 0 {
		interval = time.Millisecond
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	return interval
}

// defaultRandDuration returns a random duration in [min, max].
func defaultRandDuration(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int64N(int64(max-min)+1))
}

// scheduleNextPing anchors the next heartbeat to the session's last observed
// activity: a client that is actively using the tunnel never emits an
// application ping on a fixed schedule, and never within HeartbeatMin of real
// traffic.
func scheduleNextPing(lastActive, now time.Time, cfg PoolConfig, randDur func(min, max time.Duration) time.Duration) time.Time {
	if lastActive.IsZero() || lastActive.After(now) {
		lastActive = now
	}
	return lastActive.Add(randDur(cfg.HeartbeatMin, cfg.HeartbeatMax))
}

// heartbeatDecision computes what the pool should do with one session at the
// given instant. It is pure: the caller owns locking and state persistence.
func heartbeatDecision(now, lastActive, lastPong time.Time, st heartbeatState, cfg PoolConfig,
	randDur func(min, max time.Duration) time.Duration) (heartbeatAction, heartbeatState) {

	// A heartbeat is outstanding: either the pong arrived or the session is dead.
	if !st.awaitingSince.IsZero() {
		if lastPong.After(st.awaitingSince) {
			st.awaitingSince = time.Time{}
			st.nextPing = scheduleNextPing(lastActive, now, cfg, randDur)
			return heartbeatIdle, st
		}
		if now.Sub(st.awaitingSince) > cfg.HeartbeatTimeout {
			return heartbeatClose, st
		}
		return heartbeatIdle, st
	}

	if st.nextPing.IsZero() {
		// First observation: anchor the schedule to the session's activity.
		st.nextPing = scheduleNextPing(lastActive, now, cfg, randDur)
		return heartbeatIdle, st
	}

	// Any traffic within the last HeartbeatMin pushes the ping out; this is
	// what keeps the heartbeat from firing while the user is active.
	if !lastActive.IsZero() && now.Sub(lastActive) < cfg.HeartbeatMin {
		st.nextPing = lastActive.Add(randDur(cfg.HeartbeatMin, cfg.HeartbeatMax))
		return heartbeatIdle, st
	}

	if now.Before(st.nextPing) {
		return heartbeatIdle, st
	}

	st.awaitingSince = now
	st.nextPing = now.Add(randDur(cfg.HeartbeatMin, cfg.HeartbeatMax))
	return heartbeatSend, st
}

// heartbeatLoop runs the single keep-alive scheduler goroutine.
func (p *Pool) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatTickInterval(p.config))
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case now := <-ticker.C:
			if p.stopped.Load() {
				return
			}
			p.runHeartbeat(now)
		}
	}
}

// runHeartbeat advances the keep-alive state of every session, then performs
// the resulting sends and closes outside the pool lock.
func (p *Pool) runHeartbeat(now time.Time) {
	var sends, closes []*protocol.Session

	p.mu.Lock()

	kept := make([]*protocol.Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		if s.IsClosed() {
			delete(p.heartbeatState, s)
			continue
		}

		st := p.heartbeatState[s]
		action, next := heartbeatDecision(now, s.LastActive(), s.LastPong(), st, p.config, p.randDuration)
		switch action {
		case heartbeatSend:
			sends = append(sends, s)
			p.heartbeatState[s] = next
			kept = append(kept, s)
		case heartbeatClose:
			closes = append(closes, s)
			delete(p.heartbeatState, s)
		default:
			p.heartbeatState[s] = next
			kept = append(kept, s)
		}
	}

	// Drop keep-alive state for sessions that are no longer in the pool.
	if len(p.heartbeatState) > 0 {
		live := make(map[*protocol.Session]struct{}, len(kept))
		for _, s := range kept {
			live[s] = struct{}{}
		}
		for s := range p.heartbeatState {
			if _, ok := live[s]; !ok {
				delete(p.heartbeatState, s)
			}
		}
	}
	p.sessions = kept
	p.mu.Unlock()

	// Session writes/closes can block on the connection: never hold the pool
	// mutex while doing them.
	for _, s := range sends {
		if err := s.SendHeartbeat(); err != nil && !errors.Is(err, protocol.ErrSessionClosed) {
			log.Printf("tsunami: heartbeat send failed session=%d: %v", s.Seq(), err)
		}
	}
	for _, s := range closes {
		log.Printf("tsunami: closing unresponsive session %d (no heartbeat response within %v)", s.Seq(), p.config.HeartbeatTimeout)
		_ = s.Close()
	}
}

// CloseIdleSessions closes sessions idle longer than the given duration,
// keeping at least minKeep sessions alive. Used by Surge controller for
// proactive downgrade.
func (p *Pool) CloseIdleSessions(idleDuration time.Duration, minKeep int) {
	now := time.Now()

	p.mu.Lock()

	activeCount := 0
	for _, s := range p.sessions {
		if !s.IsClosed() {
			activeCount++
		}
	}

	remaining := make([]*protocol.Session, 0, len(p.sessions))
	closing := make([]*protocol.Session, 0)
	for _, s := range p.sessions {
		if s.IsClosed() {
			delete(p.heartbeatState, s)
			continue
		}
		if s.IsIdle() && activeCount > minKeep {
			idleSince := s.IdleSince()
			if !idleSince.IsZero() && now.Sub(idleSince) > idleDuration {
				closing = append(closing, s)
				delete(p.heartbeatState, s)
				activeCount--
				continue
			}
		}
		remaining = append(remaining, s)
	}
	p.sessions = remaining
	p.mu.Unlock()

	for _, s := range closing {
		_ = s.Close()
	}
}

// Close closes all sessions in the pool, clears the keep-alive state and stops
// the background goroutines. It is safe to call more than once.
func (p *Pool) Close() {
	if !p.stopped.CompareAndSwap(false, true) {
		return
	}
	close(p.stopCh)

	p.mu.Lock()
	sessions := p.sessions
	p.sessions = nil
	p.heartbeatState = make(map[*protocol.Session]heartbeatState)
	p.mu.Unlock()

	for _, s := range sessions {
		_ = s.Close()
	}
}
