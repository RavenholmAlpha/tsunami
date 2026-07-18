package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/control/adapters/xboardtsunami"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

func TestNodeSyncClearsUsersReloadsConfigAndRetriesTraffic(t *testing.T) {
	var mu sync.Mutex
	configRevision := "config-v1"
	users := []map[string]any{{
		"id":           "1001",
		"uuid":         "user-uuid",
		"speed_limit":  100,
		"device_limit": 2,
	}}
	reportAttempts := 0
	var reportIDs []string
	var reports []map[string][2]int64

	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-credential" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}

		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/server/tsunami/config":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"revision": configRevision,
				"config": map[string]any{
					"listen":    "127.0.0.1:0",
					"transport": map[string]any{"mode": "raw"},
				},
			})
		case "/api/v2/server/tsunami/users":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"revision": "users-" + configRevision,
				"users":    users,
			})
		case "/api/v2/server/tsunami/report":
			var report struct {
				ReportID string              `json:"report_id"`
				Traffic  map[string][2]int64 `json:"traffic"`
			}
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			reportAttempts++
			reportIDs = append(reportIDs, report.ReportID)
			reports = append(reports, report.Traffic)
			if reportAttempts == 1 {
				http.Error(w, "temporary panel failure", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer panel.Close()

	instance, err := New(Config{XBoard: xboardtsunami.Config{
		BaseURL:    panel.URL,
		NodeID:     "42",
		Credential: "node-credential",
	}})
	if err != nil {
		t.Fatalf("new node: %v", err)
	}
	defer func() {
		if err := instance.Close(); err != nil {
			t.Fatalf("close node: %v", err)
		}
	}()

	ctx := context.Background()
	if err := instance.SyncUsers(ctx); err != nil {
		t.Fatalf("initial user sync: %v", err)
	}
	if got := len(instance.UserStore().Snapshot().Users); got != 1 {
		t.Fatalf("initial user count = %d, want 1", got)
	}
	if err := instance.SyncConfig(ctx); err != nil {
		t.Fatalf("initial config sync: %v", err)
	}
	if got := instance.ConfigRevision(); got != "config-v1" {
		t.Fatalf("config revision = %q, want config-v1", got)
	}

	mu.Lock()
	users = []map[string]any{}
	configRevision = "config-v2"
	mu.Unlock()
	if err := instance.SyncUsers(ctx); err != nil {
		t.Fatalf("empty user sync: %v", err)
	}
	if got := len(instance.UserStore().Snapshot().Users); got != 0 {
		t.Fatalf("empty snapshot did not clear users, count=%d", got)
	}
	if err := instance.SyncConfig(ctx); err != nil {
		t.Fatalf("config reload: %v", err)
	}
	if got := instance.ConfigRevision(); got != "config-v2" {
		t.Fatalf("config revision = %q, want config-v2", got)
	}

	instance.UsageTracker().Record(&protocol.UserInfo{ID: "1001"}, control.DirectionUpload, 8192)
	instance.UsageTracker().Record(&protocol.UserInfo{ID: "1001"}, control.DirectionDownload, 16384)
	if err := instance.ReportUsage(ctx); err == nil {
		t.Fatal("first report succeeded despite temporary panel failure")
	}
	if err := instance.ReportUsage(ctx); err != nil {
		t.Fatalf("retry report: %v", err)

	}
	mu.Lock()
	defer mu.Unlock()
	if len(reportIDs) != 2 || reportIDs[0] == "" || reportIDs[0] != reportIDs[1] {
		t.Fatalf("report ids = %#v, want one reused non-empty id", reportIDs)
	}
	if got := reports[1]["1001"]; got != [2]int64{8192, 16384} {
		t.Fatalf("retried traffic = %#v", got)
	}
}
