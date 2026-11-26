package fronting

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// An invalid signature must not populate the replay cache: only requests that
// pass HMAC verification take the replay check. Otherwise any unauthenticated
// client could grow the cache without bound.
func TestVerifyRequestDoesNotCacheInvalidSignatures(t *testing.T) {
	key := KeyFromSecret("secret")
	cache := NewNonceCache(5 * time.Minute)
	now := time.Now()

	req, err := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	if err := SignRequest(req, key, now); err != nil {
		t.Fatal(err)
	}
	req.Header.Set(headerAuth, "HMAC-SHA256 Signature=deadbeef")
	if VerifyRequest(req, [][32]byte{key}, now, ClockSkew, cache) {
		t.Fatal("bogus signature verified")
	}
	if invalidEntries := cache.Len(); invalidEntries != 0 {
		t.Fatalf("invalid signature populated the replay cache: %d entries", invalidEntries)
	}

	valid, err := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	valid.Host = "example.com"
	if err := SignRequest(valid, key, now); err != nil {
		t.Fatal(err)
	}
	if !VerifyRequest(valid, [][32]byte{key}, now, ClockSkew, cache) {
		t.Fatal("valid signature rejected")
	}
	if validEntries := cache.Len(); validEntries != 1 {
		t.Fatalf("valid request should cache exactly one nonce, got %d", validEntries)
	}
}

func TestNonceCacheIsBounded(t *testing.T) {
	cache := NewNonceCache(5 * time.Minute)
	defer cache.Close()
	cache.maxEntries = 64
	for i := 0; i < 1000; i++ {
		cache.Add(fmt.Sprintf("nonce-%032d", i))
	}
	if size := cache.Len(); size > cache.maxEntries {
		t.Fatalf("nonce cache fallback grew to %d entries, limit %d", size, cache.maxEntries)
	}

	// Also verify the 64-shard pointer-free 32-hex path enforces bounds and replay detection.
	hexCache := NewNonceCache(5 * time.Minute)
	defer hexCache.Close()
	hexCache.maxEntries = 128 // 2 per shard across 64 shards
	for i := 0; i < 1000; i++ {
		n := fmt.Sprintf("%032x", i)
		if !hexCache.Add(n) {
			t.Fatalf("fresh hex nonce %s unexpectedly rejected", n)
		}
	}
	if size := hexCache.Len(); size > hexCache.maxEntries {
		t.Fatalf("sharded hex nonce cache grew to %d entries, limit %d", size, hexCache.maxEntries)
	}
	// Most recent nonce must be rejected as replay (both lower and upper hex case).
	last := fmt.Sprintf("%032x", 999)
	if hexCache.Add(last) {
		t.Fatalf("replay of %s unexpectedly accepted", last)
	}
	if hexCache.Add(fmt.Sprintf("%032X", 999)) {
		t.Fatalf("uppercase replay of %s unexpectedly accepted", last)
	}
}
