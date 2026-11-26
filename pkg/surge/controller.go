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
	// Default: 2
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
		Threshold:         2,
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
	expanding    atomic.Bool

	// Cooldown tracking to prevent rapid layer oscillation
	lastUpgrade   time.Time
	lastDowngrade time.Time

	// Monitoring
	stopCh       chan struct{}
	once         sync.Once
	mu           sync.Mutex
	syncExpandMu sync.Mutex
}

// NewController creates a new Surge controller.
func NewController(config Config, pool *mux.Pool) *Controller {
	config = normalizeConfig(config)
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

func normalizeConfig(config Config) Config {
	defaults := DefaultConfig()
	if config.Mode != ModeAuto && config.Mode != ModeNone {
		config.Mode = defaults.Mode
	}
	if config.Threshold <= 0 {
		config.Threshold = defaults.Threshold
	}
	if config.MaxConnections <= 0 {
		config.MaxConnections = defaults.MaxConnections
	}
	if config.IdleDowngradeTime <= 0 {
		config.IdleDowngradeTime = defaults.IdleDowngradeTime
	}
	if config.MaxConnections < 1 {
		config.MaxConnections = 1
	}
	return config
}

func (c *Controller) configSnapshot() Config {
	c.mu.Lock()
	cfg := c.config
	c.mu.Unlock()
	return cfg
}

// GetSession returns the best session for a new stream.
// In Layer 1, returns the single session (multiplexed).
// In Layer 2, returns the least-loaded session across the pool.
// When the next stream would cross the threshold on a single session, it expands
// immediately so the triggering stream lands on a fresh connection, while any
// additional connections warm in parallel in the background.
func (c *Controller) GetSession() (*protocol.Session, error) {
	decision, maxConn := c.checkExpansionForNextStream()
	switch decision {
	case expandModeNone:
		return c.pool.GetOrCreateSession()
	case expandSync:
		session, err := c.createExpansionSession(maxConn)
		if err == nil {
			c.expandAsync(maxConn)
			return session, nil
		}
		log.Printf("tsunami surge: immediate expansion failed: %v", err)
	case expandAsyncOnly:
		c.expandAsync(maxConn)
	}

	layer := c.currentLayer.Load()

	if layer == 1 {
		// Layer 1: single connection — all streams multiplexed on it
		return c.pool.GetOrCreateSession()
	}

	// Layer 2: distribute to least-loaded session without blocking
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

// ApplyServerSettings reconciles Surge limits advertised by the server in
// ServerSettings without overriding an explicit client ModeNone choice.
func (c *Controller) ApplyServerSettings(mode Mode, maxConn, threshold int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if mode == ModeNone {
		c.config.Mode = ModeNone
		c.currentLayer.Store(1)
		return
	}
	if c.config.Mode == ModeNone {
		return
	}
	if maxConn > 0 && maxConn < c.config.MaxConnections {
		c.config.MaxConnections = maxConn
	}
	if threshold > 0 {
		c.config.Threshold = threshold
	}
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

// surgeCooldown is the minimum interval between layer transitions to prevent oscillation.
const surgeCooldown = 30 * time.Second

// evaluate checks stream counts and decides on layer transitions.
// On upgrade to Layer 2, it proactively expands by creating additional sessions
// (asynchronously) up to MaxConnections.
// On downgrade to Layer 1, it closes idle sessions outside c.mu to avoid
// blocking concurrent GetSession calls on network I/O.
func (c *Controller) evaluate() {
	var (
		doExpand       bool
		expandMaxConn  int
		doDowngrade    bool
		idleDowngradeD time.Duration
	)

	c.mu.Lock()
	cfg := c.config
	if cfg.Mode == ModeNone {
		c.mu.Unlock()
		return
	}

	totalStreams := c.pool.ActiveStreamCount()
	sessionCount := c.pool.SessionCount()
	currentLayer := c.currentLayer.Load()
	now := time.Now()

	// --- Upgrade: Layer 1 → Layer 2 ---
	if currentLayer == 1 && totalStreams > cfg.Threshold {
		if c.lastDowngrade.IsZero() || now.Sub(c.lastDowngrade) >= surgeCooldown {
			if sessionCount < cfg.MaxConnections {
				log.Printf("tsunami surge: upgrading to Layer 2 (streams=%d > threshold=%d)",
					totalStreams, cfg.Threshold)
				c.currentLayer.Store(2)
				c.lastUpgrade = now
				doExpand = true
				expandMaxConn = cfg.MaxConnections
			}
		}
	} else if currentLayer == 2 && sessionCount < cfg.MaxConnections && totalStreams > cfg.Threshold {
		// --- Layer 2 active expansion: keep scaling if still under pressure ---
		doExpand = true
		expandMaxConn = cfg.MaxConnections
	} else if currentLayer == 2 && totalStreams <= cfg.Threshold/4 {
		// --- Downgrade: Layer 2 → Layer 1 ---
		// Use threshold/4 (wider hysteresis) to prevent rapid oscillation.
		if c.lastUpgrade.IsZero() || now.Sub(c.lastUpgrade) >= surgeCooldown {
			doDowngrade = true
			idleDowngradeD = cfg.IdleDowngradeTime
		}
	}
	c.mu.Unlock()

	if doExpand {
		c.expandAsync(expandMaxConn)
		return
	}

	if doDowngrade {
		// Close idle sessions (keep at least 1) outside c.mu.
		c.pool.CloseIdleSessions(idleDowngradeD, 1)

		if c.pool.SessionCount() <= 1 {
			c.mu.Lock()
			if c.currentLayer.Load() == 2 && c.pool.SessionCount() <= 1 {
				log.Printf("tsunami surge: downgrading to Layer 1 (streams=%d)", c.pool.ActiveStreamCount())
				c.currentLayer.Store(1)
				c.lastDowngrade = time.Now()
			}
			c.mu.Unlock()
		}
	}
}

type expandDecision uint8

const (
	expandNone expandDecision = iota
	expandModeNone
	expandAsyncOnly
	expandSync
)

func (c *Controller) checkExpansionForNextStream() (expandDecision, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.config.Mode == ModeNone {
		return expandModeNone, 0
	}

	totalStreams := c.pool.ActiveStreamCount()
	sessionCount := c.pool.SessionCount()
	maxConn := c.config.MaxConnections
	if sessionCount >= maxConn || sessionCount == 0 {
		return expandNone, maxConn
	}
	// Pre-warm additional connections asynchronously as soon as the stream
	// about to open reaches Threshold (zero blocking latency for caller).
	if totalStreams+1 < c.config.Threshold {
		return expandNone, maxConn
	}

	now := time.Now()
	if c.currentLayer.Load() == 1 {
		if !c.lastDowngrade.IsZero() && now.Sub(c.lastDowngrade) < surgeCooldown {
			return expandNone, maxConn
		}
		log.Printf("tsunami surge: upgrading to Layer 2 (streams=%d threshold=%d)",
			totalStreams, c.config.Threshold)
		c.currentLayer.Store(2)
		c.lastUpgrade = now
	}
	// If only 1 session exists and active streams already reached Threshold,
	// expand immediately so the caller gets an unloaded connection; once 2+
	// sessions exist or while pre-warming at Threshold-1, expand asynchronously
	// so caller streams never stall on TLS/H2 handshakes.
	if sessionCount == 1 && totalStreams >= c.config.Threshold {
		return expandSync, maxConn
	}
	return expandAsyncOnly, maxConn
}

func (c *Controller) createExpansionSession(maxConn int) (*protocol.Session, error) {
	c.syncExpandMu.Lock()
	defer c.syncExpandMu.Unlock()

	if c.pool.SessionCount() >= maxConn {
		return c.pool.GetLeastLoadedSession()
	}

	session, err := c.pool.CreateNewSession()
	if err != nil {
		return nil, err
	}
	log.Printf("tsunami surge: expanded pool immediately (%d/%d connections)",
		c.pool.SessionCount(), maxConn)
	return session, nil
}

func (c *Controller) expandAsync(maxConnections int) {
	if maxConnections <= 0 {
		maxConnections = c.configSnapshot().MaxConnections
	}
	if !c.expanding.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.expanding.Store(false)
		c.expandPool(maxConnections)
	}()
}

// expandPool asynchronously creates additional sessions in parallel up to maxConnections.
func (c *Controller) expandPool(maxConnections int) {
	cur := c.pool.SessionCount() + c.pool.DialingCount()
	need := maxConnections - cur
	if need <= 0 {
		return
	}

	var wg sync.WaitGroup
	for i := 0; i < need; i++ {
		select {
		case <-c.stopCh:
			wg.Wait()
			return
		default:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.pool.CreateNewSession(); err != nil {
				log.Printf("tsunami surge: expand pool failed: %v", err)
				return
			}
			log.Printf("tsunami surge: expanded pool (%d/%d connections)",
				c.pool.SessionCount(), maxConnections)
		}()
	}
	wg.Wait()
}

// Stop stops the Surge controller.
func (c *Controller) Stop() {
	c.once.Do(func() {
		close(c.stopCh)
	})
}
