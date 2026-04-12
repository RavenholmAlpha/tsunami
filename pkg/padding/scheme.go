// Package padding implements the TSUNAMI programmable padding system.
//
// The PaddingScheme defines how the first N packets of a session should be
// padded and/or split to defeat traffic analysis. It supports:
//   - Size-dimension: packet splitting with random-range segment sizes
//   - Keepalive: minimal idle-period background waste packets (like TLS heartbeat)
package padding

import (
	"crypto/md5"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
)

// Scheme represents a parsed PaddingScheme.
type Scheme struct {
	// Stop processing padding after this many packets (0-indexed).
	Stop int
	// Rules maps packet index to list of segments.
	Rules map[int][]Segment
	// Keepalive configuration for idle-period background waste.
	Keepalive *KeepaliveConfig
	// Raw stores the original text for MD5 computation.
	Raw string
}

// Segment represents a single segment in a packet's padding strategy.
type Segment struct {
	MinSize int  // minimum segment size in bytes
	MaxSize int  // maximum segment size in bytes
	IsCheck bool // 'c' — if user data is exhausted, stop here
}

// KeepaliveConfig defines minimal idle-period keepalive parameters.
// Sends tiny cmdWaste packets at long intervals, similar to TLS heartbeat.
// Only active when all Streams in the Session are idle.
type KeepaliveConfig struct {
	IntervalMinMs int // minimum interval between keepalive packets (ms)
	IntervalMaxMs int // maximum interval between keepalive packets (ms)
	SizeMin       int // minimum keepalive packet size (bytes)
	SizeMax       int // maximum keepalive packet size (bytes)
}

// DefaultScheme returns the default PaddingScheme.
func DefaultScheme() *Scheme {
	raw := `stop=8
0=30-30
1=100-400
2=400-500,c,500-1000,c,500-1000,c,500-1000,c,500-1000
3=9-9,500-1000
4=500-1000
5=500-1000
6=500-1000
7=500-1000
keepalive=30000-60000:4-8`
	scheme, _ := Parse(raw)
	return scheme
}

// Parse parses a PaddingScheme text definition.
func Parse(text string) (*Scheme, error) {
	s := &Scheme{
		Stop:  8,
		Rules: make(map[int][]Segment),
		Raw:   text,
	}

	lines := strings.Split(strings.TrimSpace(text), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]

		switch {
		case key == "stop":
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("padding: invalid stop value: %w", err)
			}
			s.Stop = n

		case key == "keepalive":
			// keepalive=30000-60000:4-8
			kp := strings.SplitN(val, ":", 2)
			if len(kp) != 2 {
				continue
			}
			interval, err := parseRange(kp[0])
			if err != nil {
				continue
			}
			size, err := parseRange(kp[1])
			if err != nil {
				continue
			}
			s.Keepalive = &KeepaliveConfig{
				IntervalMinMs: interval[0],
				IntervalMaxMs: interval[1],
				SizeMin:       size[0],
				SizeMax:       size[1],
			}

		default:
			// Packet index rule: 0=30-30 or 2=400-500,c,500-1000,...
			idx, err := strconv.Atoi(key)
			if err != nil {
				continue // skip unknown keys
			}
			segments, err := parseSegments(val)
			if err != nil {
				return nil, fmt.Errorf("padding: invalid rule for packet %d: %w", idx, err)
			}
			s.Rules[idx] = segments
		}
	}

	return s, nil
}

// parseSegments parses a comma-separated segment list like "400-500,c,500-1000".
func parseSegments(val string) ([]Segment, error) {
	parts := strings.Split(val, ",")
	var segments []Segment
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "c" {
			segments = append(segments, Segment{IsCheck: true})
			continue
		}
		r, err := parseRange(p)
		if err != nil {
			return nil, err
		}
		segments = append(segments, Segment{MinSize: r[0], MaxSize: r[1]})
	}
	return segments, nil
}

// parseRange parses "min-max" into [min, max].
func parseRange(s string) ([2]int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return [2]int{}, fmt.Errorf("padding: invalid range: %s", s)
	}
	min, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return [2]int{}, err
	}
	max, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return [2]int{}, err
	}
	return [2]int{min, max}, nil
}

// MD5 returns the lowercase hex MD5 digest of the scheme's raw text.
func (s *Scheme) MD5() string {
	h := md5.Sum([]byte(s.Raw))
	return fmt.Sprintf("%x", h)
}

// GetSegments returns the segment list for a given packet index, or nil if not defined.
func (s *Scheme) GetSegments(packetIdx int) []Segment {
	if packetIdx >= s.Stop {
		return nil
	}
	return s.Rules[packetIdx]
}

// RandomInRange returns a random integer in [min, max].
// Uses math/rand/v2 which is automatically seeded from a secure source in Go 1.22+.
func RandomInRange(min, max int) int {
	if min >= max {
		return min
	}
	return min + rand.IntN(max-min+1)
}

// Encode serializes the scheme back to text format.
func (s *Scheme) Encode() string {
	return s.Raw
}
