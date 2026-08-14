package xboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
)

func TestAdapterFetchSnapshotAndSubmitUsage(t *testing.T) {
	var reported struct {
		NodeID  string `json:"node_id"`
		Traffic []struct {
			UserID   string `json:"user_id"`
			Upload   int64  `json:"u"`
			Download int64  `json:"d"`
		} `json:"traffic"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Node-Key") != "Token panel-secret" {
			t.Fatalf("authorization = %q", r.Header.Get("X-Node-Key"))
		}
		if r.URL.Query().Get("node_id") != "42" {
			t.Fatalf("node_id = %q", r.URL.Query().Get("node_id"))
		}

		switch r.URL.Path {
		case "/private/users":
			if r.Method != http.MethodGet {
				t.Fatalf("users method = %s", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
              "status":"success",
              "data":{"version":"rev-7","users":[{
                "id":1001,
                "email":"alice@example.test",
                "token_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                "speed_limit_bps":4096,
                "transfer_enable":1048576,
                "u":12,
                "d":34,
                "expired_at":1893456000
              }]}
            }`))
		case "/private/traffic":
			if r.Method != http.MethodPost {
				t.Fatalf("traffic method = %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
				t.Fatalf("decode traffic: %v", err)
			}
			_, _ = w.Write([]byte(`{"status":"success"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter, err := New(Config{
		BaseURL:             server.URL,
		APIKey:              "panel-secret",
		NodeID:              "42",
		UsersPath:           "/private/users",
		TrafficPath:         "/private/traffic",
		AuthorizationHeader: "X-Node-Key",
		AuthorizationPrefix: "Token ",
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	snapshot, err := adapter.FetchSnapshot(context.Background())
	if err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}
	if snapshot.Version != "rev-7" || len(snapshot.Users) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	user := snapshot.Users[0]
	if user.ID != "1001" || user.Name != "alice@example.test" || user.SpeedLimitBps != 4096 || user.QuotaBytes != 1048576 {
		t.Fatalf("mapped user = %+v", user)
	}
	if user.UsedUploadBytes != 12 || user.UsedDownloadBytes != 34 {
		t.Fatalf("mapped usage = %+v", user)
	}
	if user.ExpiresAt.Before(time.Unix(1893456000, 0)) {
		t.Fatalf("expiry = %v", user.ExpiresAt)
	}

	err = adapter.SubmitUsage(context.Background(), []control.UsageDelta{
		{
			UserID:        "1001",
			UploadBytes:   128,
			DownloadBytes: 256,
		},
	})
	if err != nil {
		t.Fatalf("submit usage: %v", err)
	}
	if reported.NodeID != "42" || len(reported.Traffic) != 1 {
		t.Fatalf("reported usage = %+v", reported)
	}
	entry := reported.Traffic[0]
	if entry.UserID != "1001" || entry.Upload != 128 || entry.Download != 256 {
		t.Fatalf("reported entry = %+v", entry)
	}
}

func TestAdapterRejectsInvalidPanelUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","users":[{"id":1}]}`))
	}))
	defer server.Close()

	adapter, err := New(Config{BaseURL: server.URL, APIKey: "key", NodeID: "1"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if _, err := adapter.FetchSnapshot(context.Background()); err == nil {
		t.Fatal("expected invalid user error")
	}
}

func TestAdapterUsesRawKeyForCustomAuthorizationHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "panel-secret" {
			t.Fatalf("X-API-Key = %q, want raw API key", got)
		}
		_, _ = w.Write([]byte(`{"success":true,"users":[]}`))
	}))
	defer server.Close()

	adapter, err := New(Config{
		BaseURL:             server.URL,
		APIKey:              "panel-secret",
		NodeID:              "42",
		AuthorizationHeader: "X-API-Key",
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if _, err := adapter.FetchSnapshot(context.Background()); err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}
}

func TestAdapterAcceptsStringUnixExpiry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
          "success":true,
          "users":[{
            "id":"1001",
            "token_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "expired_at":"1893456000"
          }]
        }`))
	}))
	defer server.Close()

	adapter, err := New(Config{BaseURL: server.URL, APIKey: "key", NodeID: "42"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	snapshot, err := adapter.FetchSnapshot(context.Background())
	if err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}
	if got, want := snapshot.Users[0].ExpiresAt, time.Unix(1893456000, 0); !got.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got, want)
	}
}
