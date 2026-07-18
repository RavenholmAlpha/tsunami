package xboardtsunami

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

func TestAdapterEnrollFetchAndReport(t *testing.T) {
	var report struct {
		ReportID string              `json:"report_id"`
		Traffic  map[string][2]int64 `json:"traffic"`
		Alive    map[string][]string `json:"alive"`
	}
	var credentials []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case enrollPath:
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode enrollment: %v", err)
			}
			if request["node_id"] != "42" || request["enrollment_token"] != "enroll-secret" {
				t.Fatalf("enrollment request = %#v", request)
			}
			_, _ = w.Write([]byte(`{"node_id":"42","credential":"node-credential"}`))
		case configPath:
			credentials = append(credentials, r.Header.Get("Authorization"))
			w.Header().Set("ETag", `"config-v1"`)
			_, _ = w.Write([]byte(`{"revision":"config-v1","config":{"listen":":443","tls":{"cert_file":"cert.pem","key_file":"key.pem"},"transport":{"mode":"raw"}}}`))
		case usersPath:
			credentials = append(credentials, r.Header.Get("Authorization"))
			w.Header().Set("ETag", `"users-v1"`)
			_, _ = w.Write([]byte(`{"revision":"users-v1","users":[{"id":1001,"uuid":"user-uuid","speed_limit":100,"device_limit":3}]}`))
		case reportPath:
			credentials = append(credentials, r.Header.Get("Authorization"))
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			_, _ = w.Write([]byte(`{"accepted":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	credentialFile := filepath.Join(t.TempDir(), "node.credential")
	adapter, err := New(Config{
		BaseURL:         server.URL,
		NodeID:          "42",
		EnrollmentToken: "enroll-secret",
		CredentialFile:  credentialFile,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	remote, unchanged, err := adapter.FetchConfig(context.Background())
	if err != nil || unchanged {
		t.Fatalf("fetch config unchanged=%v err=%v", unchanged, err)
	}
	if remote.Revision != "config-v1" || remote.Listen != ":443" {
		t.Fatalf("remote config = %+v", remote)
	}
	stored, err := os.ReadFile(credentialFile)
	if err != nil || string(stored) != "node-credential\n" {
		t.Fatalf("credential=%q err=%v", stored, err)
	}

	snapshot, unchanged, err := adapter.FetchSnapshot(context.Background())
	if err != nil || unchanged || len(snapshot.Users) != 1 {
		t.Fatalf("fetch users snapshot=%+v unchanged=%v err=%v", snapshot, unchanged, err)
	}
	user := snapshot.Users[0]
	hash := sha256.Sum256([]byte("user-uuid"))
	if user.TokenHash != hex.EncodeToString(hash[:]) || user.SpeedLimitBps != 12_500_000 || user.MaxDevices != 3 {
		t.Fatalf("mapped user = %+v", user)
	}

	if err := adapter.SubmitReport(context.Background(), "report-1", []control.UsageDelta{{
		UserID: "1001", UploadBytes: 8192, DownloadBytes: 16384,
	}}); err != nil {
		t.Fatalf("submit report: %v", err)
	}
	if report.ReportID != "report-1" || report.Traffic["1001"] != [2]int64{8192, 16384} {
		t.Fatalf("report = %+v", report)
	}
	for _, credential := range credentials {
		if credential != "Bearer node-credential" {
			t.Fatalf("credential header = %q", credential)
		}
	}
}

func TestAdapterHandlesNotModifiedAndEmptyUserSnapshot(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-credential" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		requestCount++
		if requestCount == 1 {
			w.Header().Set("ETag", `"users-v1"`)
			_, _ = w.Write([]byte(`{"revision":"users-v1","users":[]}`))
			return
		}
		if r.Header.Get("If-None-Match") != `"users-v1"` {
			t.Fatalf("if-none-match = %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	adapter, err := New(Config{BaseURL: server.URL, NodeID: "42", Credential: "node-credential"})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	snapshot, unchanged, err := adapter.FetchSnapshot(context.Background())
	if err != nil || unchanged || snapshot == nil || len(snapshot.Users) != 0 {
		t.Fatalf("first users snapshot=%+v unchanged=%v err=%v", snapshot, unchanged, err)
	}
	snapshot, unchanged, err = adapter.FetchSnapshot(context.Background())
	if err != nil || !unchanged || snapshot != nil {
		t.Fatalf("second users snapshot=%+v unchanged=%v err=%v", snapshot, unchanged, err)
	}
}

func TestSessionLeaseTracksUnlimitedUsersAndReleasesLimitedUsers(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer node-credential" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case admitPath, renewPath:
			_, _ = w.Write([]byte(`{"allowed":true}`))
		case releasePath:
			_, _ = w.Write([]byte(`{"released":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter, err := New(Config{BaseURL: server.URL, NodeID: "42", Credential: "node-credential", RequestTimeout: time.Second})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	limited := &protocol.UserInfo{ID: "1", MaxDevices: 2}
	lease, err := adapter.Admit(context.Background(), limited, "203.0.113.7", "session-00000001")
	if err != nil {
		t.Fatalf("admit limited: %v", err)
	}
	if err := lease.Renew(context.Background()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := adapter.aliveSnapshot(); len(got["1"]) != 0 {
		t.Fatalf("alive after release = %#v", got)
	}

	unlimited := &protocol.UserInfo{ID: "2"}
	lease, err = adapter.Admit(context.Background(), unlimited, "203.0.113.8", "session-00000002")
	if err != nil {
		t.Fatalf("admit unlimited: %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("release unlimited: %v", err)
	}
	if len(calls) != 3 || calls[0] != admitPath || calls[1] != renewPath || calls[2] != releasePath {
		t.Fatalf("lease calls = %#v", calls)
	}
}
