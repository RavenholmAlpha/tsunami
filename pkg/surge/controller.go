// Package surge implements the TSUNAMI Surge congestion control system.
//
// Surge uses a layered design:
//   - Layer 1 (default): All streams share a single TLS connection
//   - Layer 2 (auto-upgrade): When concurrent streams exceed a threshold,
//     additional TLS connections are opened automatically
//
// Each stream always stays on a single connection — no packet reordering.
// All connections go through a single port (443).
package surge

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// Mode defines the Surge operating mode.
type Mode string

const (
	// ModeNone disables Surge entirely. Pure single connection.
	ModeNone Mode = "none"
	// ModeAuto is the default. Layer 1 single connection +
	// auto-upgrade to Layer 2 multi-connection when concurrent streams > threshold.
	ModeAuto Mode = "auto"
)

// Config holds Surge controller configuration.
type Config struct {
	// Mode is the Surge operating mode.
	Mode Mode
	// Threshold is the number of concurrent streams that triggers Layer 2 upgrade.
	// Default: 8
	Threshold int
	// MaxConnections is the maximum number of TLS connections in Layer 2.
	// Default: 4
	MaxConnections int
	// IdleDowngradeTime is how long a session must be idle before being closed
	// during Layer 2 downgrade. Default: 30s
	IdleDowngradeTime time.Duration
}

// DefaultConfig returns the default Surge configuration.
func DefaultConfig() Config {
	return Config{
		Mode:              ModeAuto,
		Threshold:         8,
		MaxConnections:    4,
		IdleDowngradeTime: 30 * time.Second,
	}
}

// Controller manages the Surge connection scaling.
type Controller struct {
	config Config
	pool   *mux.Pool

	// State tracking
	currentLayer atomic.Int32 // 1 or 2

	// Monitoring
	stopCh chan struct{}
	once   sync.Once
	mu     sync.Mutex
}

// NewController creates a new Surge controller.
func NewController(config Config, pool *mux.Pool) *Controller {
	c := &Controller{
		config: config,
		pool:   pool,
		stopCh: make(chan struct{}),
	}
	c.currentLayer.Store(1)

	if config.Mode == ModeAuto {
		go c.monitor()
	}

	return c
}

// GetSession returns the best session for a new stream.
// In Layer 1, returns the single session (multiplexed).
// In Layer 2, returns the least-loaded session across the pool.
// Session expansion is handled by the background evaluate() loop, not here.
func (c *Controller) GetSession() (*protocol.Session, error) {
	if c.config.Mode == ModeNone {
		return c.pool.GetOrCreateSession()
	}

	layer := c.currentLayer.Load()

	if layer == 1 {
		// Layer 1: single connection — all streams multiplexed on it
		return c.pool.GetOrCreateSession()
	}

	// Layer 2: distribute to least-loaded session
	session, err := c.pool.GetLeastLoadedSession()
	if err != nil {
		// No sessions yet — create the first one
		return c.pool.GetOrCreateSession()
	}

	return session, nil
}

// CurrentLayer returns the current Surge layer (1 or 2).
func (c *Controller) CurrentLayer() int {
	return int(c.currentLayer.Load())
}

// monitor periodically checks whether to upgrade/downgrade layers.
func (c *Controller) monitor() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.evaluate()
		}
	}
}

// evaluate checks stream counts and decides on layer transitions.
// On upgrade to Layer 2, it proactively expands by creating additional sessions
// (asynchronously) up to MaxConnections.
// On downgrade to Layer 1, it closes idle sessions to reduce resource usage.
func (c *Controller) evaluate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	totalStreams := c.pool.ActiveStreamCount()
	sessionCount := c.pool.SessionCount()
	currentLayer := c.currentLayer.Load()

	// --- Upgrade: Layer 1 → Layer 2 ---
	if currentLayer == 1 && totalStreams > c.config.Threshold {
		if sessionCount < c.config.MaxConnections {
			log.Printf("tsunami surge: upgrading to Layer 2 (streams=%d > threshold=%d)",
				totalStreams, c.config.Threshold)
			c.currentLayer.Store(2)

			// Proactively expand: create sessions up to MaxConnections
			needed := c.config.MaxConnections - sessionCount
			go c.expandPool(needed)
		}
	}

	// --- Layer 2 active expansion: keep scaling if still under pressure ---
	if currentLayer == 2 && sessionCount < c.config.MaxConnections && totalStreams > c.config.Threshold {
		needed := c.config.MaxConnections - sessionCount
		go c.expandPool(needed)
	}

	// --- Downgrade: Layer 2 → Layer 1 ---
	if currentLayer == 2 && totalStreams <= c.config.Threshold/2 {
		// Close idle sessions (keep at least 1)
		c.pool.CloseIdleSessions(c.config.IdleDowngradeTime, 1)

		// If we've reduced to a single session, downgrade to Layer 1
		if c.pool.SessionCount() <= 1 {
			log.Printf("tsunami surge: downgrading to Layer 1 (streams=%d)", totalStreams)
			c.currentLayer.Store(1)
		}
	}
}

// expandPool asynchronously creates additional sessions up to MaxConnections.
func (c *Controller) expandPool(needed int) {
	for i := 0; i < needed; i++ {
		select {
		case <-c.stopCh:
			return
		default:
		}

		// Re-check pool size before each creation to avoid overshooting
		if c.pool.SessionCount() >= c.config.MaxConnections {
			return
		}

		_, err := c.pool.CreateNewSession()
		if err != nil {
			log.Printf("tsunami surge: expand pool failed: %v", err)
			return
		}
		log.Printf("tsunami surge: expanded pool (%d/%d connections)",
			c.pool.SessionCount(), c.config.MaxConnections)
	}
}

// Stop stops the Surge controller.
func (c *Controller) Stop() {
	c.once.Do(func() {
		close(c.stopCh)
	})
}
