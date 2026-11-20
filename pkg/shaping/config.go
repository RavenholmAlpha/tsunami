// Package shaping provides traffic shaping for anti-traffic-analysis.
// When enabled, outgoing writes are encapsulated into randomized self-framed
// packets while remaining silent during idle periods except for occasional
// sparse, variable-sized chaff bursts. This avoids the synthetic constant-rate
// packet-stream fingerprint on HTTPS connections while still masking exact
// payload boundaries and idle timing.
package shaping

import "time"

// Config controls the traffic shaping behavior.
type Config struct {
	// FrameSize is the maximum size of each shaped data packet on the wire
	// (bytes). Outgoing writes are chunked to fit within FrameSize and
	// padded with pseudo-random bytes up to a randomized target <= FrameSize.
	// Default: 1200 (fits in a single TLS record without fragmentation).
	FrameSize int

	// Interval is the pacing interval between bursts when buffered write data
	// exceeds BurstSlots frames.
	// Default: 5ms.
	Interval time.Duration

	// BurstSlots is how many data frames can be sent back-to-back per
	// interval when draining buffered writes.
	// Default: 32.
	BurstSlots int

	// MaxBufferSize bounds the internal write buffer in bytes. Writes apply
	// backpressure once full instead of growing unboundedly.
	// Default: 4 MiB.
	MaxBufferSize int

	// IdleChaffMinInterval and IdleChaffMaxInterval bound the random interval
	// between sparse chaff bursts while the connection is idle.
	// IdleChaffMaxInterval <= 0 disables idle chaff completely.
	// Defaults: 15s .. 60s.
	IdleChaffMinInterval time.Duration
	IdleChaffMaxInterval time.Duration

	// IdleChaffMinSize and IdleChaffMaxSize bound the payload size (bytes)
	// of each idle dummy frame. Defaults: 64 .. 1200.
	IdleChaffMinSize int
	IdleChaffMaxSize int

	// IdleChaffMaxBurst is the maximum number of dummy frames sent in one
	// idle chaff wakeup (actual count is random in [1, IdleChaffMaxBurst]).
	// Default: 3.
	IdleChaffMaxBurst int

	// IdleChaffBurstMinDelay and IdleChaffBurstMaxDelay bound the random delay
	// between dummy frames within a single idle chaff burst.
	// Defaults: 10ms .. 120ms.
	IdleChaffBurstMinDelay time.Duration
	IdleChaffBurstMaxDelay time.Duration
}

// DefaultConfig returns a balanced shaping configuration: idle-silent with
// sparse, variable-sized random chaff and bounded buffering.
func DefaultConfig() Config {
	return Config{
		FrameSize:              1200,
		Interval:               5 * time.Millisecond,
		BurstSlots:             32,
		MaxBufferSize:          4 << 20,
		IdleChaffMinInterval:   15 * time.Second,
		IdleChaffMaxInterval:   60 * time.Second,
		IdleChaffMinSize:       64,
		IdleChaffMaxSize:       1200,
		IdleChaffMaxBurst:      3,
		IdleChaffBurstMinDelay: 10 * time.Millisecond,
		IdleChaffBurstMaxDelay: 120 * time.Millisecond,
	}
}

// HighThroughputConfig returns a shaping config optimized for higher bandwidth.
func HighThroughputConfig() Config {
	return Config{
		FrameSize:              1400,
		Interval:               2 * time.Millisecond,
		BurstSlots:             64,
		MaxBufferSize:          8 << 20,
		IdleChaffMinInterval:   20 * time.Second,
		IdleChaffMaxInterval:   75 * time.Second,
		IdleChaffMinSize:       64,
		IdleChaffMaxSize:       1400,
		IdleChaffMaxBurst:      3,
		IdleChaffBurstMinDelay: 10 * time.Millisecond,
		IdleChaffBurstMaxDelay: 120 * time.Millisecond,
	}
}
