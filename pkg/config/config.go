// Package config handles configuration file loading for the TSUNAMI server.
// JSON format is supported out-of-box using the Go standard library.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

// ServerConfig represents the JSON configuration file structure.
//
// Example JSON config:
//
//	{
//	  "server": {
//	    "listen": ":443",
//	    "tls": {
//	      "cert": "/path/to/cert.pem",
//	      "key": "/path/to/key.pem"
//	    },
//	    "users": [
//	      { "id": "u_1001", "name": "alice", "token_hash": "...", "bandwidth": 0 }
//	    ],
//	    "surge": {
//	      "mode": "auto",
//	      "max_connections": 4,
//	      "threshold": 8
//	    },
//	    "fallback": "127.0.0.1:8080",
//	    "padding_scheme": "stop=8\n0=30-30\n..."
//	  }
//	}
type ServerConfig struct {
	Server struct {
		Listen string `json:"listen" yaml:"listen"`

		TLS struct {
			Cert string `json:"cert" yaml:"cert"`
			Key  string `json:"key" yaml:"key"`
			ACME *struct {
				Domain string `json:"domain" yaml:"domain"`
				Email  string `json:"email" yaml:"email"`
			} `json:"acme,omitempty" yaml:"acme,omitempty"`
		} `json:"tls" yaml:"tls"`

		Users []struct {
			ID       string `json:"id" yaml:"id"`
			Name     string `json:"name" yaml:"name"`
			Password string `json:"password" yaml:"password"`

			TokenHash string `json:"token_hash" yaml:"token-hash"`

			Disabled  bool      `json:"disabled" yaml:"disabled"`
			ExpiresAt time.Time `json:"expires_at" yaml:"expires-at"`

			Bandwidth     int   `json:"bandwidth" yaml:"bandwidth"`
			SpeedLimitBps int64 `json:"speed_limit_bps" yaml:"speed-limit-bps"`

			QuotaBytes        int64 `json:"quota_bytes" yaml:"quota-bytes"`
			UsedUploadBytes   int64 `json:"used_upload_bytes" yaml:"used-upload-bytes"`
			UsedDownloadBytes int64 `json:"used_download_bytes" yaml:"used-download-bytes"`

			MaxSessions int `json:"max_sessions" yaml:"max-sessions"`
			MaxDevices  int `json:"max_devices" yaml:"max-devices"`

			Metadata map[string]string `json:"metadata" yaml:"metadata"`
		} `json:"users" yaml:"users"`

		Surge struct {
			Mode           string `json:"mode" yaml:"mode"`
			MaxConnections int    `json:"max_connections" yaml:"max-connections"`
			Threshold      int    `json:"threshold" yaml:"threshold"`
		} `json:"surge" yaml:"surge"`

		Fallback string `json:"fallback" yaml:"fallback"`
		Fronting struct {
			Enabled      bool   `json:"enabled" yaml:"enabled"`
			Path         string `json:"path" yaml:"path"`
			Secret       string `json:"secret" yaml:"secret"`
			ServerHeader string `json:"server_header" yaml:"server-header"`
			SiteName     string `json:"site_name" yaml:"site-name"`
		} `json:"fronting" yaml:"fronting"`
		PaddingScheme string `json:"padding_scheme" yaml:"padding-scheme"`
	} `json:"server" yaml:"server"`
}

// LoadFile loads a JSON configuration file.
func LoadFile(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file %s: %w", path, err)
	}

	cfg := &ServerConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse JSON %s: %w", path, err)
	}

	// Validate required fields
	if len(cfg.Server.Users) == 0 {
		return nil, fmt.Errorf("config: at least one user is required")
	}
	for i, u := range cfg.Server.Users {
		if u.Password == "" && u.TokenHash == "" {
			return nil, fmt.Errorf("config: user[%d] (%s) password or token_hash is required", i, u.Name)
		}
	}

	return cfg, nil
}

// ToServerConfig converts the parsed config into a server.Config.
func (c *ServerConfig) ToServerConfig() server.Config {
	cfg := server.Config{
		Listen: c.Server.Listen,
		TLS: transport.TLSConfig{
			CertFile:   c.Server.TLS.Cert,
			KeyFile:    c.Server.TLS.Key,
			ALPN:       []string{"h2"},
			MinVersion: 0x0304, // TLS 1.3
		},
		TCP:            *transport.DefaultTCPConfig(),
		SurgeMode:      c.Server.Surge.Mode,
		MaxConnections: c.Server.Surge.MaxConnections,
		SurgeThreshold: c.Server.Surge.Threshold,
		FallbackAddr:   c.Server.Fallback,
		Fronting: fronting.Config{
			Enabled:      c.Server.Fronting.Enabled,
			Path:         c.Server.Fronting.Path,
			Secret:       c.Server.Fronting.Secret,
			ServerHeader: c.Server.Fronting.ServerHeader,
			SiteName:     c.Server.Fronting.SiteName,
		},
		PaddingScheme: strings.TrimSpace(c.Server.PaddingScheme),
	}

	// Apply defaults
	if cfg.Listen == "" {
		cfg.Listen = ":443"
	}
	if cfg.SurgeMode == "" {
		cfg.SurgeMode = "auto"
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 4
	}
	if cfg.SurgeThreshold == 0 {
		cfg.SurgeThreshold = 8
	}

	for _, u := range c.Server.Users {
		cfg.Users = append(cfg.Users, &protocol.UserInfo{
			ID:                u.ID,
			Name:              u.Name,
			Password:          u.Password,
			TokenHash:         u.TokenHash,
			Disabled:          u.Disabled,
			ExpiresAt:         u.ExpiresAt,
			Bandwidth:         u.Bandwidth,
			SpeedLimitBps:     u.SpeedLimitBps,
			QuotaBytes:        u.QuotaBytes,
			UsedUploadBytes:   u.UsedUploadBytes,
			UsedDownloadBytes: u.UsedDownloadBytes,
			MaxSessions:       u.MaxSessions,
			MaxDevices:        u.MaxDevices,
			Metadata:          u.Metadata,
		})
	}

	return cfg
}
