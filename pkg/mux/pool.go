// Package mux implements the TSUNAMI session pool and connection multiplexing.
package mux

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// PoolConfig configures the session pool behavior.
type PoolConfig struct {
	// MinIdleSession is the minimum number of idle sessions to keep alive.
	MinIdleSession int
	// IdleCheckInterval is how often to check for idle sessions.
	IdleCheckInterval time.Duration
	// IdleTimeout is how long a session can be idle before being closed.
	IdleTimeout time.Duration
}

// DefaultPoolConfig returns sensible defaults.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MinIdleSession:    1,
		IdleCheckInterval: 30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// Pool manages a pool of reusable Sessions for connection multiplexing.
type Pool struct {
	config     PoolConfig
	sessions   []*protocol.Session
	mu         sync.Mutex
	seqCounter atomic.Uint64
	stopCh     chan struct{}
	stopped    atomic.Bool

	// Dial function to create new TLS connections
	dialFn func() (*protocol.Session, error)
}

// NewPool creates a new session pool.
func NewPool(config PoolConfig, dialFn func() (*protocol.Session, error)) *Pool {
	p := &Pool{
		config: config,
		dialFn: dialFn,
		stopCh: make(chan struct{}),
	}
	go p.idleChecker()
	return p
}

// GetOrCreateSession returns an idle session or creates a new one.
// Strategy: prefer the session with the highest Seq (newest).
func (p *Pool) GetOrCreateSession() (*protocol.Session, error) {
	p.mu.Lock()

	// Find the newest idle session
	var best *protocol.Session
	for _, s := range p.sessions {
		if s.IsClosed() {
			continue
		}
		if s.IsIdle() && (best == nil || s.Seq() > best.Seq()) {
			best = s
		}
	}

	if best != nil {
		p.mu.Unlock()
		return best, nil
	}

	p.mu.Unlock()

	// No idle session available — create a new one
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
	session, err := p.dialFn()
	if err != nil {
		return nil, fmt.Errorf("tsunami: dial new session: %w", err)
	}

	p.mu.Lock()
	p.sessions = append(p.sessions, session)
	p.mu.Unlock()

	return session, nil
}

// AddSession adds an externally created session to the pool.
func (p *Pool) AddSession(s *protocol.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions = append(p.sessions, s)
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
			p.cleanupIdle()
		}
	}
}

// cleanupIdle removes sessions that have been idle too long.
func (p *Pool) cleanupIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	idleCount := 0

	// First pass: count idle sessions
	for _, s := range p.sessions {
		if !s.IsClosed() && s.IsIdle() {
			idleCount++
		}
	}

	// Second pass: close old idle sessions (keep minIdleSession alive)
	remaining := make([]*protocol.Session, 0, len(p.sessions))
	closeable := idleCount - p.config.MinIdleSession

	for _, s := range p.sessions {
		if s.IsClosed() {
			continue // skip already closed
		}

		if s.IsIdle() && closeable > 0 {
			idleSince := s.IdleSince()
			if !idleSince.IsZero() && now.Sub(idleSince) > p.config.IdleTimeout {
				s.Close()
				closeable--
				continue
			}
		}

		remaining = append(remaining, s)
	}

	p.sessions = remaining
}

// Close closes all sessions in the pool and stops the idle checker.
func (p *Pool) Close() {
	if p.stopped.Load() {
		return
	}
	p.stopped.Store(true)
	close(p.stopCh)

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, s := range p.sessions {
		s.Close()
	}
	p.sessions = nil
}
