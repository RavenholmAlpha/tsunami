package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFile(t *testing.T) {
	jsonContent := `{
  "server": {
    "listen": ":8443",
    "tls": {
      "cert": "/etc/ssl/cert.pem",
      "key": "/etc/ssl/key.pem"
    },
    "users": [
      { "name": "alice", "password": "alice-pass", "bandwidth": 100 },
      { "name": "bob", "password": "bob-pass" }
    ],
    "surge": {
      "mode": "auto",
      "max_connections": 6,
      "threshold": 10
    },
    "fallback": "127.0.0.1:8080",
    "padding_scheme": "stop=4\n0=50-50"
  }
}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	// Verify parsed fields
	if cfg.Server.Listen != ":8443" {
		t.Errorf("listen = %q, want %q", cfg.Server.Listen, ":8443")
	}
	if cfg.Server.TLS.Cert != "/etc/ssl/cert.pem" {
		t.Errorf("tls.cert = %q, want %q", cfg.Server.TLS.Cert, "/etc/ssl/cert.pem")
	}
	if len(cfg.Server.Users) != 2 {
		t.Fatalf("users count = %d, want 2", len(cfg.Server.Users))
	}
	if cfg.Server.Users[0].Name != "alice" || cfg.Server.Users[0].Bandwidth != 100 {
		t.Errorf("user[0] = %+v", cfg.Server.Users[0])
	}
	if cfg.Server.Surge.MaxConnections != 6 {
		t.Errorf("surge.max_connections = %d, want 6", cfg.Server.Surge.MaxConnections)
	}
	if cfg.Server.Surge.Threshold != 10 {
		t.Errorf("surge.threshold = %d, want 10", cfg.Server.Surge.Threshold)
	}
	if cfg.Server.Fallback != "127.0.0.1:8080" {
		t.Errorf("fallback = %q", cfg.Server.Fallback)
	}

	// Verify ToServerConfig
	scfg := cfg.ToServerConfig()
	if scfg.Listen != ":8443" {
		t.Errorf("server config listen = %q", scfg.Listen)
	}
	if len(scfg.Users) != 2 {
		t.Errorf("server config users = %d", len(scfg.Users))
	}
	if scfg.MaxConnections != 6 {
		t.Errorf("server config max connections = %d", scfg.MaxConnections)
	}
	if scfg.FallbackAddr != "127.0.0.1:8080" {
		t.Errorf("server config fallback = %q", scfg.FallbackAddr)
	}

	t.Logf("✅ Config load test passed")
}

func TestLoadFileDefaults(t *testing.T) {
	jsonContent := `{
  "server": {
    "users": [
      { "name": "default", "password": "test-pass" }
    ]
  }
}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(jsonContent), 0644)

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	scfg := cfg.ToServerConfig()
	if scfg.Listen != ":443" {
		t.Errorf("default listen = %q, want %q", scfg.Listen, ":443")
	}
	if scfg.SurgeMode != "auto" {
		t.Errorf("default surge mode = %q, want %q", scfg.SurgeMode, "auto")
	}
	if scfg.MaxConnections != 4 {
		t.Errorf("default max connections = %d, want 4", scfg.MaxConnections)
	}
	if scfg.SurgeThreshold != 8 {
		t.Errorf("default surge threshold = %d, want 8", scfg.SurgeThreshold)
	}

	t.Logf("✅ Config defaults test passed")
}

func TestLoadFilePanelUserFields(t *testing.T) {
	tokenHash := sha256.Sum256([]byte("panel-token"))
	jsonContent := `{
  "server": {
    "users": [
      {
        "id": "u_1001",
        "name": "alice",
        "token_hash": "` + hex.EncodeToString(tokenHash[:]) + `",
        "speed_limit_bps": 1048576,
        "quota_bytes": 1073741824,
        "max_sessions": 2,
        "metadata": { "source": "panel" }
      }
    ]
  }
}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(jsonContent), 0644)

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	scfg := cfg.ToServerConfig()
	if len(scfg.Users) != 1 {
		t.Fatalf("users = %d, want 1", len(scfg.Users))
	}
	user := scfg.Users[0]
	if user.ID != "u_1001" || user.TokenHash == "" {
		t.Fatalf("user identity/hash not mapped: %+v", user)
	}
	if user.SpeedLimitBps != 1048576 || user.QuotaBytes != 1073741824 || user.MaxSessions != 2 {
		t.Fatalf("panel fields not mapped: %+v", user)
	}
	if user.Metadata["source"] != "panel" {
		t.Fatalf("metadata not mapped: %+v", user.Metadata)
	}
}

func TestLoadFileValidation(t *testing.T) {
	// No users → error
	noUsers := `{ "server": {} }`
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte(noUsers), 0644)

	_, err := LoadFile(path)
	if err == nil {
		t.Error("expected error for config with no users")
	}

	// Empty password → error
	emptyPass := `{ "server": { "users": [{ "name": "x", "password": "" }] } }`
	os.WriteFile(path, []byte(emptyPass), 0644)

	_, err = LoadFile(path)
	if err == nil {
		t.Error("expected error for user with empty password")
	}

	t.Logf("✅ Config validation test passed")
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := LoadFile("/nonexistent/config.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadFileBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json {{{"), 0644)

	_, err := LoadFile(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}
