package protocol

import (
	"fmt"
	"strconv"
	"strings"
)

// ClientSettings represents the settings sent by the client in cmdSettings.
type ClientSettings struct {
	Version            int
	Client             string
	PaddingFingerprint string
	SurgeBandwidth     int // Mbps, 0 = disabled
}

// EncodeClientSettings serializes client settings to the wire format.
func EncodeClientSettings(s *ClientSettings) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "v=%d\n", s.Version)
	fmt.Fprintf(&b, "client=%s\n", s.Client)
	if s.PaddingFingerprint != "" {
		fmt.Fprintf(&b, "padding-fingerprint=%s\n", s.PaddingFingerprint)
	}
	if s.SurgeBandwidth > 0 {
		fmt.Fprintf(&b, "surge-bandwidth=%d", s.SurgeBandwidth)
	}
	return []byte(b.String())
}

// DecodeClientSettings parses client settings from wire format.
func DecodeClientSettings(data []byte) (*ClientSettings, error) {
	s := &ClientSettings{Version: 1}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "v":
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("tsunami: invalid version: %w", err)
			}
			s.Version = v
		case "client":
			s.Client = val
		case "padding-fingerprint":
			s.PaddingFingerprint = val
		case "surge-bandwidth":
			bw, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("tsunami: invalid surge-bandwidth: %w", err)
			}
			s.SurgeBandwidth = bw
		}
	}
	return s, nil
}

// SurgeMode defines the server's allowed Surge congestion control mode.
type SurgeMode string

const (
	SurgeModeNone SurgeMode = "none"
	SurgeModeAuto SurgeMode = "auto"
)

// ServerSettings represents the settings sent by the server in cmdServerSettings.
type ServerSettings struct {
	Version        int
	SurgeMode      SurgeMode
	MaxConnections int
	Threshold      int // concurrent stream threshold for Layer 2 upgrade
}

// EncodeServerSettings serializes server settings to the wire format.
func EncodeServerSettings(s *ServerSettings) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "v=%d\n", s.Version)
	if s.SurgeMode != "" {
		fmt.Fprintf(&b, "surge-mode=%s\n", string(s.SurgeMode))
	}
	if s.MaxConnections > 0 {
		fmt.Fprintf(&b, "max-connections=%d\n", s.MaxConnections)
	}
	if s.Threshold > 0 {
		fmt.Fprintf(&b, "threshold=%d", s.Threshold)
	}
	return []byte(b.String())
}

// DecodeServerSettings parses server settings from wire format.
func DecodeServerSettings(data []byte) (*ServerSettings, error) {
	s := &ServerSettings{Version: 1, SurgeMode: SurgeModeAuto}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "v":
			v, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("tsunami: invalid version: %w", err)
			}
			s.Version = v
		case "surge-mode":
			s.SurgeMode = SurgeMode(val)
		case "max-connections":
			mc, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("tsunami: invalid max-connections: %w", err)
			}
			s.MaxConnections = mc
		case "threshold":
			th, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("tsunami: invalid threshold: %w", err)
			}
			s.Threshold = th
		}
	}
	return s, nil
}
