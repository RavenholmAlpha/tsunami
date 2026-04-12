// Package config handles configuration file loading for the TSUNAMI server.
// JSON format is supported out-of-box using the Go standard library.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
//	      { "name": "alice", "password": "strong-password", "bandwidth": 0 }
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
			Password  string `json:"password" yaml:"password"`
			Name      string `json:"name" yaml:"name"`
			Bandwidth int    `json:"bandwidth" yaml:"bandwidth"`
		} `json:"users" yaml:"users"`

		Surge struct {
			Mode           string `json:"mode" yaml:"mode"`
			MaxConnections int    `json:"max_connections" yaml:"max-connections"`
			Threshold      int    `json:"threshold" yaml:"threshold"`
		} `json:"surge" yaml:"surge"`

		Fallback      string `json:"fallback" yaml:"fallback"`
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
		if u.Password == "" {
			return nil, fmt.Errorf("config: user[%d] (%s) password is required", i, u.Name)
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
		PaddingScheme:  strings.TrimSpace(c.Server.PaddingScheme),
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
			Name:      u.Name,
			Password:  u.Password,
			Bandwidth: u.Bandwidth,
		})
	}

	return cfg
}
