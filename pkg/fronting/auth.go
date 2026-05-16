package fronting

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	headerVersion = "X-Api-Version"
	headerDate    = "X-Date"
	headerNonce   = "X-Trace-Id"
	headerAuth    = "Authorization"
	authVersion   = "v1"
)

// NonceCache tracks recently seen nonces to prevent replay attacks.
type NonceCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
}

// NewNonceCache creates a nonce cache with the given TTL and starts a
// background goroutine to evict expired entries.
func NewNonceCache(ttl time.Duration) *NonceCache {
	nc := &NonceCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
	}
	go nc.evictLoop()
	return nc
}

// Add records a nonce. Returns false if the nonce was already seen (replay).
func (nc *NonceCache) Add(nonce string) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	if _, exists := nc.entries[nonce]; exists {
		return false
	}
	nc.entries[nonce] = time.Now()
	return true
}

func (nc *NonceCache) evictLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		nc.mu.Lock()
		cutoff := time.Now().Add(-nc.ttl)
		for k, t := range nc.entries {
			if t.Before(cutoff) {
				delete(nc.entries, k)
			}
		}
		nc.mu.Unlock()
	}
}

// StripAuthHeaders removes fronting-only request metadata before a decoy proxy
// forwards unauthenticated traffic to a normal origin.
func StripAuthHeaders(h http.Header) {
	h.Del(headerVersion)
	h.Del(headerDate)
	h.Del(headerNonce)
	h.Del(headerAuth)
}

// SignRequest adds HTTP-layer authentication headers.
func SignRequest(req *http.Request, key [32]byte, now time.Time) error {
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(now.Unix(), 10)

	req.Header.Set(headerVersion, authVersion)
	req.Header.Set(headerDate, ts)
	req.Header.Set(headerNonce, nonce)
	sig := computeMAC(req.Method, req.URL.EscapedPath(), req.Host, ts, nonce, key)
	req.Header.Set(headerAuth, "HMAC-SHA256 Signature="+sig)
	return nil
}

// VerifyRequest checks HTTP-layer auth against any accepted key.
// The nonceCache parameter prevents replay attacks; pass nil to skip replay protection.
func VerifyRequest(req *http.Request, keys [][32]byte, now time.Time, skew time.Duration, nonceCache ...*NonceCache) bool {
	if len(keys) == 0 {
		return false
	}
	if req.Header.Get(headerVersion) != authVersion {
		return false
	}
	ts := req.Header.Get(headerDate)
	nonce := req.Header.Get(headerNonce)
	authHeader := req.Header.Get(headerAuth)
	if ts == "" || nonce == "" || authHeader == "" {
		return false
	}
	got := strings.TrimPrefix(authHeader, "HMAC-SHA256 Signature=")
	if got == authHeader {
		return false
	}
	if len(nonce) < 16 || len(nonce) > 128 {
		return false
	}
	timestamp, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	when := time.Unix(timestamp, 0)
	if when.After(now.Add(skew)) || when.Before(now.Add(-skew)) {
		return false
	}
	if _, err := hex.DecodeString(got); err != nil {
		return false
	}

	// Replay protection: reject previously seen nonces
	if len(nonceCache) > 0 && nonceCache[0] != nil {
		if !nonceCache[0].Add(nonce) {
			return false
		}
	}

	requestPath := req.URL.EscapedPath()
	if requestPath == "" {
		requestPath = req.URL.Path
	}
	for _, key := range keys {
		want := computeMAC(req.Method, requestPath, req.Host, ts, nonce, key)
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

func computeMAC(method, requestPath, host, ts, nonce string, key [32]byte) string {
	mac := hmac.New(sha256.New, key[:])
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s", method, requestPath, strings.ToLower(host), ts, nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

func randomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fronting: nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
