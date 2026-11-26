package fronting

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
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

type nonceRecord struct {
	key        string
	insertedAt time.Time
}

// NonceCache tracks recently seen nonces to prevent replay attacks in O(1) amortized time.
type NonceCache struct {
	mu         sync.Mutex
	entries    map[string]time.Time
	ring       []nonceRecord
	ringHead   int
	ringLen    int
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
	now := time.Now()
	nc.pruneLocked(now)
	if exp, exists := nc.entries[nonce]; exists {
		if !exp.Before(now.Add(-nc.ttl)) {
			return false
		}
		delete(nc.entries, nonce)
	}
	if nc.maxEntries > 0 {
		for len(nc.entries) >= nc.maxEntries {
			if !nc.popOldestLocked() {
				for k := range nc.entries {
					delete(nc.entries, k)
					break
				}
			}
		}
	}
	nc.entries[nonce] = now
	nc.pushRingLocked(nonce, now)
	return true
}

func (nc *NonceCache) pushRingLocked(nonce string, now time.Time) {
	if nc.ringLen == len(nc.ring) {
		newCap := len(nc.ring) * 2
		if newCap < 64 {
			newCap = 64
		}
		if nc.maxEntries > 0 && newCap > nc.maxEntries {
			newCap = nc.maxEntries
		}
		if newCap <= len(nc.ring) {
			newCap = len(nc.ring) + 1
		}
		newRing := make([]nonceRecord, newCap)
		for i := 0; i < nc.ringLen; i++ {
			newRing[i] = nc.ring[(nc.ringHead+i)%len(nc.ring)]
		}
		nc.ring = newRing
		nc.ringHead = 0
	}
	tail := (nc.ringHead + nc.ringLen) % len(nc.ring)
	nc.ring[tail] = nonceRecord{key: nonce, insertedAt: now}
	nc.ringLen++
}

func (nc *NonceCache) popOldestLocked() bool {
	if nc.ringLen == 0 {
		return false
	}
	rec := nc.ring[nc.ringHead]
	nc.ring[nc.ringHead] = nonceRecord{}
	nc.ringHead = (nc.ringHead + 1) % len(nc.ring)
	nc.ringLen--
	if exp, ok := nc.entries[rec.key]; ok && exp.Equal(rec.insertedAt) {
		delete(nc.entries, rec.key)
	}
	return true
}

// pruneLocked removes expired entries from the head of the FIFO ring in O(1) amortized time.
// nc.mu must be held.
func (nc *NonceCache) pruneLocked(now time.Time) {
	cutoff := now.Add(-nc.ttl)
	for nc.ringLen > 0 {
		head := nc.ring[nc.ringHead]
		if !head.insertedAt.Before(cutoff) {
			break
		}
		nc.popOldestLocked()
	}
	if nc.ringLen == 0 && len(nc.entries) > 0 {
		for k, t := range nc.entries {
			if t.Before(cutoff) {
				delete(nc.entries, k)
			}
		}
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

var staticAuthVersionSlice = []string{authVersion}

// SignRequest adds HTTP-layer authentication headers. When HeaderSessionID or
// HeaderStreamSeq are set on req, they are bound into the HMAC signature.
func SignRequest(req *http.Request, key [32]byte, now time.Time) error {
	var rawNonce [16]byte
	if _, err := rand.Read(rawNonce[:]); err != nil {
		return fmt.Errorf("fronting: nonce: %w", err)
	}
	var nonceHex [32]byte
	hex.Encode(nonceHex[:], rawNonce[:])
	nonce := string(nonceHex[:])

	var tsBuf [24]byte
	ts := string(strconv.AppendInt(tsBuf[:0], now.Unix(), 10))

	sid := req.Header.Get(HeaderSessionID)
	seq := req.Header.Get(HeaderStreamSeq)

	req.Header[headerVersion] = staticAuthVersionSlice
	req.Header[headerDate] = []string{ts}
	req.Header[headerNonce] = []string{nonce}
	sum := computeMACBytes(req.Method, req.URL.EscapedPath(), req.Host, ts, nonce, sid, seq, key)
	const sigPrefix = "HMAC-SHA256 Signature="
	var authBuf [len(sigPrefix) + sha256.Size*2]byte
	copy(authBuf[:], sigPrefix)
	hex.Encode(authBuf[len(sigPrefix):], sum[:])
	req.Header[headerAuth] = []string{string(authBuf[:])}
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
	const sigPrefix = "HMAC-SHA256 Signature="
	if len(authHeader) <= len(sigPrefix) || authHeader[:len(sigPrefix)] != sigPrefix {
		return false
	}
	got := authHeader[len(sigPrefix):]
	if len(got) != sha256.Size*2 {
		return false
	}
	var gotMAC [sha256.Size]byte
	if !decodeHex32(got, &gotMAC) {
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

	sid := req.Header.Get(HeaderSessionID)
	seq := req.Header.Get(HeaderStreamSeq)
	if sid != "" {
		if len(sid) < 16 || len(sid) > 64 || len(sid)%2 != 0 || !isHexString(sid) {
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
		want := computeMACBytes(req.Method, requestPath, req.Host, ts, nonce, sid, seq, key)
		if subtle.ConstantTimeCompare(gotMAC[:], want[:]) == 1 {
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

func computeMACBytes(method, requestPath, host, ts, nonce, sid, seq string, key [32]byte) [sha256.Size]byte {
	need := len(method) + 1 + len(requestPath) + 1 + len(host) + 1 + len(ts) + 1 + len(nonce)
	hasSplit := sid != "" || seq != ""
	if hasSplit {
		need += 2 + len(sid) + len(seq)
	}

	var innerStack [sha256.BlockSize + 512]byte
	var inner []byte
	if sha256.BlockSize+need <= len(innerStack) {
		inner = innerStack[:sha256.BlockSize]
	} else {
		inner = make([]byte, sha256.BlockSize, sha256.BlockSize+need)
	}
	for i := 0; i < len(key); i++ {
		inner[i] = key[i] ^ 0x36
	}
	for i := len(key); i < sha256.BlockSize; i++ {
		inner[i] = 0x36
	}

	inner = append(inner, method...)
	inner = append(inner, '\n')
	inner = append(inner, requestPath...)
	inner = append(inner, '\n')
	for i := 0; i < len(host); i++ {
		c := host[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		inner = append(inner, c)
	}
	inner = append(inner, '\n')
	inner = append(inner, ts...)
	inner = append(inner, '\n')
	inner = append(inner, nonce...)
	if hasSplit {
		inner = append(inner, '\n')
		inner = append(inner, sid...)
		inner = append(inner, '\n')
		inner = append(inner, seq...)
	}

	innerSum := sha256.Sum256(inner)

	var outer [sha256.BlockSize + sha256.Size]byte
	for i := 0; i < len(key); i++ {
		outer[i] = key[i] ^ 0x5c
	}
	for i := len(key); i < sha256.BlockSize; i++ {
		outer[i] = 0x5c
	}
	copy(outer[sha256.BlockSize:], innerSum[:])
	return sha256.Sum256(outer[:])
}

func decodeHexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		if _, ok := decodeHexNibble(s[i]); !ok {
			return false
		}
	}
	return true
}

func decodeHex32(s string, dst *[sha256.Size]byte) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < sha256.Size; i++ {
		hi, ok1 := decodeHexNibble(s[i*2])
		lo, ok2 := decodeHexNibble(s[i*2+1])
		if !ok1 || !ok2 {
			return false
		}
		dst[i] = (hi << 4) | lo
	}
	return true
}
