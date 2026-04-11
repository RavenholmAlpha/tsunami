// Package config handles YAML configuration file loading for the TSUNAMI server.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

// ServerConfig represents the YAML configuration file structure.
type ServerConfig struct {
	Server struct {
		Listen string `yaml:"listen"`

		TLS struct {
			Cert string `yaml:"cert"`
			Key  string `yaml:"key"`
			ACME *struct {
				Domain string `yaml:"domain"`
				Email  string `yaml:"email"`
			} `yaml:"acme,omitempty"`
		} `yaml:"tls"`

		Users []struct {
			Password  string `yaml:"password"`
			Name      string `yaml:"name"`
			Bandwidth int    `yaml:"bandwidth"`
		} `yaml:"users"`

		Surge struct {
			Mode           string `yaml:"mode"`
			MaxConnections int    `yaml:"max-connections"`
			Threshold      int    `yaml:"threshold"`
		} `yaml:"surge"`

		Fallback      string `yaml:"fallback"`
		PaddingScheme string `yaml:"padding-scheme"`
	} `yaml:"server"`
}

// LoadFile loads a YAML configuration file.
// Note: This is a simple key=value parser since we avoid external dependencies.
// For production, use gopkg.in/yaml.v3.
func LoadFile(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file: %w", err)
	}
	_ = data
	// TODO: Parse YAML properly. For now, use CLI flags.
	return nil, fmt.Errorf("config: YAML parsing requires gopkg.in/yaml.v3 dependency")
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

	for _, u := range c.Server.Users {
		cfg.Users = append(cfg.Users, &protocol.UserInfo{
			Name:      u.Name,
			Password:  u.Password,
			Bandwidth: u.Bandwidth,
		})
	}

	return cfg
}
