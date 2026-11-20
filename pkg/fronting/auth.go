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

	// HeaderSessionID identifies a Split-H2 session across GET downlink and
	// POST uplink requests.
	HeaderSessionID = "X-Session-Id"
	// HeaderStreamSeq carries the monotonic chunk sequence number for Split-H2
	// uplink POST requests.
	HeaderStreamSeq = "X-Stream-Seq"

	// defaultNonceCacheMaxEntries bounds the replay cache. Only requests with
	// a valid signature are inserted, so the cache normally grows with the
	// (small) rate of new authenticated tunnels; the cap is a safety net
	// against a client that holds the fronting secret and floods requests.
	defaultNonceCacheMaxEntries = 1 << 17
)

// NonceCache tracks recently seen nonces to prevent replay attacks.
type NonceCache struct {
	mu         sync.Mutex
	entries    map[string]time.Time
	ttl        time.Duration
	maxEntries int
}

// NewNonceCache creates a nonce cache with the given TTL and starts a
// background goroutine to evict expired entries.
func NewNonceCache(ttl time.Duration) *NonceCache {
	nc := &NonceCache{
		entries:    make(map[string]time.Time),
		ttl:        ttl,
		maxEntries: defaultNonceCacheMaxEntries,
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
	nc.pruneLocked(time.Now())
	if nc.maxEntries > 0 && len(nc.entries) >= nc.maxEntries {
		nc.evictOldestLocked(nc.maxEntries / 8)
	}
	nc.entries[nonce] = time.Now()
	return true
}

// pruneLocked removes expired entries. nc.mu must be held.
func (nc *NonceCache) pruneLocked(now time.Time) {
	cutoff := now.Add(-nc.ttl)
	for k, t := range nc.entries {
		if t.Before(cutoff) {
			delete(nc.entries, k)
		}
	}
}

func (nc *NonceCache) evictOldestLocked(count int) {
	if count < 1 {
		count = 1
	}
	for k := range nc.entries {
		if count == 0 {
			break
		}
		delete(nc.entries, k)
		count--
	}
}

func (nc *NonceCache) evictLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		nc.mu.Lock()
		nc.pruneLocked(time.Now())
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
	h.Del(HeaderSessionID)
	h.Del(HeaderStreamSeq)
}

// SignRequest adds HTTP-layer authentication headers. When HeaderSessionID or
// HeaderStreamSeq are set on req, they are bound into the HMAC signature.
func SignRequest(req *http.Request, key [32]byte, now time.Time) error {
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(now.Unix(), 10)

	sid := req.Header.Get(HeaderSessionID)
	seq := req.Header.Get(HeaderStreamSeq)

	req.Header.Set(headerVersion, authVersion)
	req.Header.Set(headerDate, ts)
	req.Header.Set(headerNonce, nonce)
	sig := computeMAC(req.Method, req.URL.EscapedPath(), req.Host, ts, nonce, sid, seq, key)
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

	sid := req.Header.Get(HeaderSessionID)
	seq := req.Header.Get(HeaderStreamSeq)
	if sid != "" {
		if len(sid) < 16 || len(sid) > 64 {
			return false
		}
		if _, err := hex.DecodeString(sid); err != nil {
			return false
		}
	}
	if seq != "" {
		if sid == "" {
			return false
		}
		if _, err := strconv.ParseUint(seq, 10, 64); err != nil {
			return false
		}
	}

	requestPath := req.URL.EscapedPath()
	if requestPath == "" {
		requestPath = req.URL.Path
	}

	valid := false
	for _, key := range keys {
		want := computeMAC(req.Method, requestPath, req.Host, ts, nonce, sid, seq, key)
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(want)) == 1 {
			valid = true
			break
		}
	}
	if !valid {
		return false
	}

	if len(nonceCache) > 0 && nonceCache[0] != nil {
		if !nonceCache[0].Add(nonce) {
			return false
		}
	}
	return true
}

func computeMAC(method, requestPath, host, ts, nonce, sid, seq string, key [32]byte) string {
	mac := hmac.New(sha256.New, key[:])
	if sid == "" && seq == "" {
		fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s", method, requestPath, strings.ToLower(host), ts, nonce)
	} else {
		fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s\n%s\n%s", method, requestPath, strings.ToLower(host), ts, nonce, sid, seq)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func randomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fronting: nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
