// Package config handles configuration file loading for the TSUNAMI server.
// JSON format is supported out-of-box using the Go standard library.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control/adapters/xboardtsunami"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/node"
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
//	    "fronting": {
//	      "enabled": true,
//	      "path": "/assets/update",
//	      "secret": "front-secret",
//	      "site_name": "Example",
//	      "site": {
//	        "mode": "local",                  // builtin | local | remote
//	        "upstream": "http://127.0.0.1:8080",
//	        "preserve_host": true,            // optional; local=true, remote=false
//	        "insecure_skip_verify": false     // optional; HTTPS upstream only
//	      }
//	    },
//	    "padding_scheme": "stop=6\nstream_bytes=12288\nctl=~40-160\nc0=~80-200\nc1=~900-1200\ns0=~2600-4200\n..."
//	  }
//	}
//
// The deprecated "decoy_proxy": "http://127.0.0.1:8080" key is still accepted
// as an alias for a site upstream and is migrated into the site block on load.
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
			enabledSet   bool   // distinguishes an omitted field from an explicit false
			Path         string `json:"path" yaml:"path"`
			Secret       string `json:"secret" yaml:"secret"`
			ServerHeader string `json:"server_header" yaml:"server-header"`
			SiteName     string `json:"site_name" yaml:"site-name"`
			// DecoyProxy is deprecated: configure Site.Upstream instead. It is
			// still accepted for backward compatibility.
			DecoyProxy string `json:"decoy_proxy" yaml:"decoy-proxy"`
			Site       struct {
				Mode               string `json:"mode" yaml:"mode"`
				Upstream           string `json:"upstream" yaml:"upstream"`
				PreserveHost       *bool  `json:"preserve_host" yaml:"preserve-host"`
				InsecureSkipVerify bool   `json:"insecure_skip_verify" yaml:"insecure-skip-verify"`
			} `json:"site" yaml:"site"`
		} `json:"fronting" yaml:"fronting"`
		PaddingScheme string `json:"padding_scheme" yaml:"padding-scheme"`
		AllowAll      bool   `json:"allow_all" yaml:"allow-all"`
		MaxStreams    int    `json:"max_streams" yaml:"max-streams"`
	} `json:"server" yaml:"server"`

	// XBoard is used only by tsunami-node. The panel owns all data-plane
	// settings so no server block is required in a node configuration.
	XBoard struct {
		BaseURL         string `json:"base_url" yaml:"base-url"`
		NodeID          string `json:"node_id" yaml:"node-id"`
		EnrollmentToken string `json:"enrollment_token" yaml:"enrollment-token"`
		Credential      string `json:"credential" yaml:"credential"`
		CredentialFile  string `json:"credential_file" yaml:"credential-file"`
		SyncInterval    string `json:"sync_interval" yaml:"sync-interval"`
		ConfigInterval  string `json:"config_interval" yaml:"config-interval"`
		RequestTimeout  string `json:"request_timeout" yaml:"request-timeout"`
	} `json:"xboard" yaml:"xboard"`
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
	var presence struct {
		Server struct {
			Fronting map[string]json.RawMessage `json:"fronting"`
		} `json:"server"`
	}
	if err := json.Unmarshal(data, &presence); err != nil {
		return nil, fmt.Errorf("config: parse JSON %s: %w", path, err)
	}
	_, cfg.Server.Fronting.enabledSet = presence.Server.Fronting["enabled"]

	// Validate required fields
	if len(cfg.Server.Users) == 0 && cfg.XBoard.BaseURL == "" {
		return nil, fmt.Errorf("config: at least one user is required")
	}
	for i, u := range cfg.Server.Users {
		if u.Password == "" && u.TokenHash == "" {
			return nil, fmt.Errorf("config: user[%d] (%s) password or token_hash is required", i, u.Name)
		}
	}

	return cfg, nil
}

// ToNodeConfig converts the file configuration to a panel-managed node.
// XBoard users intentionally replace static users at runtime.
func (c *ServerConfig) ToNodeConfig() (node.Config, error) {
	if c == nil {
		return node.Config{}, fmt.Errorf("config: nil configuration")
	}

	syncInterval, err := parseDuration("xboard.sync_interval", c.XBoard.SyncInterval)
	if err != nil {
		return node.Config{}, err
	}
	configInterval, err := parseDuration("xboard.config_interval", c.XBoard.ConfigInterval)
	if err != nil {
		return node.Config{}, err
	}
	requestTimeout, err := parseDuration("xboard.request_timeout", c.XBoard.RequestTimeout)
	if err != nil {
		return node.Config{}, err
	}

	return node.Config{
		XBoard: xboardtsunami.Config{
			BaseURL:         c.XBoard.BaseURL,
			NodeID:          c.XBoard.NodeID,
			EnrollmentToken: c.XBoard.EnrollmentToken,
			Credential:      c.XBoard.Credential,
			CredentialFile:  c.XBoard.CredentialFile,
			SyncInterval:    syncInterval,
			ConfigInterval:  configInterval,
			RequestTimeout:  requestTimeout,
		},
	}, nil
}

func parseDuration(field, value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("config: parse %s: %w", field, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("config: %s must be positive", field)
	}
	return duration, nil
}

// ToServerConfig converts the parsed config into a server.Config.
func (c *ServerConfig) ToServerConfig() server.Config {
	frontingEnabled := true
	if c.Server.Fronting.enabledSet {
		frontingEnabled = c.Server.Fronting.Enabled
	}

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
			Enabled:      frontingEnabled,
			Path:         c.Server.Fronting.Path,
			Secret:       c.Server.Fronting.Secret,
			ServerHeader: c.Server.Fronting.ServerHeader,
			SiteName:     c.Server.Fronting.SiteName,
			DecoyProxy:   c.Server.Fronting.DecoyProxy,
			Site: fronting.SiteConfig{
				Mode:               fronting.SiteMode(c.Server.Fronting.Site.Mode),
				Upstream:           c.Server.Fronting.Site.Upstream,
				PreserveHost:       c.Server.Fronting.Site.PreserveHost,
				InsecureSkipVerify: c.Server.Fronting.Site.InsecureSkipVerify,
			},
		},
		PaddingScheme: strings.TrimSpace(c.Server.PaddingScheme),
		AllowAll:      c.Server.AllowAll,
		MaxStreams:    c.Server.MaxStreams,
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
