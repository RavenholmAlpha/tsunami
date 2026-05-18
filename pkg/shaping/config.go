// Package shaping provides constant-rate traffic shaping for anti-traffic-analysis.
// When enabled, it normalizes the byte stream into fixed-size packets sent at
// a constant interval, filling idle periods with dummy data. This makes the
// connection indistinguishable from a constant-bitrate stream to a network observer.
package shaping

import "time"

// Config controls the traffic shaping behavior.
type Config struct {
	// FrameSize is the fixed size of each shaped packet on the wire (bytes).
	// All outgoing writes are chunked/padded to exactly this size.
	// Default: 1200 (fits in a single TLS record without fragmentation).
	FrameSize int

	// Interval is the fixed time between each outgoing packet.
	// Determines the shaped bitrate: FrameSize * 8 / Interval = bps.
	// Default: 5ms (= 1.92 Mbps with 1200-byte frames).
	Interval time.Duration

	// BurstSlots is how many frame slots can be sent back-to-back when
	// buffered data exceeds one frame. This allows real throughput to burst
	// above the base rate while still maintaining fixed packet sizes.
	// Default: 32 (= ~61 Mbps burst with 1200-byte frames at 5ms interval).
	BurstSlots int
}

// DefaultConfig returns a conservative shaping configuration.
func DefaultConfig() Config {
	return Config{
		FrameSize:  1200,
		Interval:   5 * time.Millisecond,
		BurstSlots: 32,
	}
}

// HighThroughputConfig returns a shaping config optimized for higher bandwidth.
func HighThroughputConfig() Config {
	return Config{
		FrameSize:  1400,
		Interval:   2 * time.Millisecond,
		BurstSlots: 64,
	}
}
