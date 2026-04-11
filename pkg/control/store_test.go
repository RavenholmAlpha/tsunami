package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

func TestUserStoreAuthenticateAndValidity(t *testing.T) {
	store, err := NewUserStore([]*protocol.UserInfo{
		{ID: "alice", Name: "Alice", Password: "alice-pass"},
		{ID: "bob", Name: "Bob", Password: "bob-pass", Disabled: true},
		{ID: "carol", Name: "Carol", Password: "carol-pass", ExpiresAt: time.Now().Add(-time.Minute)},
	})
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}

	if user := store.Authenticate(protocol.PasswordHash("alice-pass")); user == nil || user.ID != "alice" {
		t.Fatalf("alice should authenticate, got %+v", user)
	}
	if user := store.Authenticate(protocol.PasswordHash("bob-pass")); user != nil {
		t.Fatalf("disabled bob should not authenticate")
	}
	if user := store.Authenticate(protocol.PasswordHash("carol-pass")); user != nil {
		t.Fatalf("expired carol should not authenticate")
	}
}

func TestUserStoreTokenHashAndIncrementalUpdate(t *testing.T) {
	tokenHash := sha256.Sum256([]byte("panel-token"))
	store := NewEmptyUserStore()

	if err := store.ApplySnapshot(&Snapshot{
		Version: "1",
		Full:    true,
		Users: []*protocol.UserInfo{
			{ID: "u1", Name: "u1", TokenHash: hex.EncodeToString(tokenHash[:])},
			{ID: "u2", Name: "u2", Password: "old-pass"},
		},
	}); err != nil {
		t.Fatalf("ApplySnapshot full: %v", err)
	}

	if user := store.Authenticate(tokenHash); user == nil || user.ID != "u1" {
		t.Fatalf("token hash user should authenticate, got %+v", user)
	}

	if err := store.ApplySnapshot(&Snapshot{
		Version:        "2",
		Full:           false,
		DeletedUserIDs: []string{"u2"},
		Users: []*protocol.UserInfo{
			{ID: "u3", Name: "u3", Password: "new-pass"},
		},
	}); err != nil {
		t.Fatalf("ApplySnapshot incremental: %v", err)
	}

	if store.Version() != "2" {
		t.Fatalf("version = %q, want 2", store.Version())
	}
	if user := store.Authenticate(protocol.PasswordHash("old-pass")); user != nil {
		t.Fatalf("deleted user should not authenticate")
	}
	if user := store.Authenticate(protocol.PasswordHash("new-pass")); user == nil || user.ID != "u3" {
		t.Fatalf("new user should authenticate, got %+v", user)
	}
}

func TestPipelineMiddleware(t *testing.T) {
	adapter := NewStaticAdapter("test", []*protocol.UserInfo{
		{Name: "alice", Password: "alice-pass"},
	})
	pipeline, err := NewPipeline(adapter, NormalizeUsers(), ValidateUsers())
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	snapshot, err := pipeline.FetchSnapshot(context.Background())
	if err != nil {
		t.Fatalf("FetchSnapshot: %v", err)
	}
	if snapshot.Users[0].ID != "alice" {
		t.Fatalf("normalized ID = %q, want alice", snapshot.Users[0].ID)
	}
}

func TestValidateUsersRejectsDuplicateHash(t *testing.T) {
	snapshot := &Snapshot{
		Full: true,
		Users: []*protocol.UserInfo{
			{ID: "u1", Password: "same-pass"},
			{ID: "u2", Password: "same-pass"},
		},
	}
	if err := ApplyMiddleware(context.Background(), snapshot, ValidateUsers()); err == nil {
		t.Fatalf("expected duplicate hash error")
	}
}
