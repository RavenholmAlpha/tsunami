package server

import (
	"testing"

	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// fronting must use a single HMAC key. Adding every user password would make
// unauthenticated verification cost O(users) HMACs and widen the fronting
// credential to every account.
func TestBuildFrontingKeysUsesSingleKey(t *testing.T) {
	users := []*protocol.UserInfo{
		{Name: "alice", Password: "alice-password"},
		{Name: "bob", Password: "bob-password"},
	}
	keys, err := buildFrontingKeys(Config{
		Fronting: fronting.Config{Enabled: true, Secret: "shared-fronting-secret"},
		Users:    users,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("explicit secret: got %d keys, want 1", len(keys))
	}
	if keys[0] != fronting.KeyFromSecret("shared-fronting-secret") {
		t.Fatal("explicit secret key mismatch")
	}

	keys, err = buildFrontingKeys(Config{
		Fronting: fronting.Config{Enabled: true},
		Users:    users,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("password fallback: got %d keys, want 1", len(keys))
	}
	if keys[0] != fronting.KeyFromSecret("alice-password") {
		t.Fatal("password fallback should use the first user only")
	}
}
