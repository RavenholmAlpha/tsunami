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

	// defaultNonceCacheMaxEntries bounds the replay cache across all shards.
	// Only requests with a valid signature are inserted, so the cap is a safety
	// net against a client holding the fronting secret flooding requests.
	defaultNonceCacheMaxEntries = 1 << 18
	nonceCacheShards            = 64
)

// nonceBinRecord is pointer-free so Go's GC marks the ring slice and shard maps
// noscan, eliminating GC pause overhead even with hundreds of thousands of cached nonces.
type nonceBinRecord struct {
	key        [16]byte
	insertedAt int64
}

type nonceShard struct {
	mu       sync.Mutex
	entries  map[[16]byte]int64
	ring     []nonceBinRecord
	ringHead int
	ringLen  int
	_        [8]byte // pad to 64-byte cache line on 64-bit platforms
}

type nonceStrRecord struct {
	key        string
	insertedAt int64
}

type nonceStrShard struct {
	mu       sync.Mutex
	entries  map[string]int64
	ring     []nonceStrRecord
	ringHead int
	ringLen  int
}

// NonceCache tracks recently seen nonces to prevent replay attacks in O(1) amortized time.
// Standard 32-hex (128-bit) nonces are stored in 64 pointer-free shards for zero-GC-scan
// lock-striped concurrency; non-standard nonce formats use a bounded fallback shard.
type NonceCache struct {
	shards     [nonceCacheShards]nonceShard
	fallback   nonceStrShard
	ttl        time.Duration
	maxEntries int
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// NewNonceCache creates a nonce cache with the given TTL and starts a
// background goroutine to evict expired entries.
func NewNonceCache(ttl time.Duration) *NonceCache {
	nc := &NonceCache{
		ttl:        ttl,
		maxEntries: defaultNonceCacheMaxEntries,
		stopCh:     make(chan struct{}),
	}
	go nc.evictLoop()
	return nc
}

// Close stops the background eviction goroutine cleanly.
func (nc *NonceCache) Close() error {
	if nc == nil {
		return nil
	}
	nc.stopOnce.Do(func() {
		if nc.stopCh != nil {
			close(nc.stopCh)
		}
	})
	return nil
}

// Len returns the current number of cached nonces across all shards.
func (nc *NonceCache) Len() int {
	if nc == nil {
		return 0
	}
	total := 0
	for i := range nc.shards {
		sh := &nc.shards[i]
		sh.mu.Lock()
		total += len(sh.entries)
		sh.mu.Unlock()
	}
	nc.fallback.mu.Lock()
	total += len(nc.fallback.entries)
	nc.fallback.mu.Unlock()
	return total
}

func (nc *NonceCache) shardMaxEntries() int {
	if nc.maxEntries <= 0 {
		return 0
	}
	m := nc.maxEntries / nonceCacheShards
	if m < 1 {
		m = 1
	}
	return m
}

// Add records a nonce. Returns false if the nonce was already seen (replay).
func (nc *NonceCache) Add(nonce string) bool {
	nowNano := time.Now().UnixNano()
	cutoff := nowNano - nc.ttl.Nanoseconds()

	var raw [16]byte
	if len(nonce) == 32 && decodeHex16(nonce, &raw) {
		sh := &nc.shards[raw[0]&(nonceCacheShards-1)]
		maxPerShard := nc.shardMaxEntries()
		sh.mu.Lock()
		ok := sh.addLocked(raw, nowNano, cutoff, maxPerShard)
		sh.mu.Unlock()
		return ok
	}

	nc.fallback.mu.Lock()
	ok := nc.fallback.addLocked(nonce, nowNano, cutoff, nc.maxEntries)
	nc.fallback.mu.Unlock()
	return ok
}

func (sh *nonceShard) addLocked(key [16]byte, nowNano, cutoff int64, maxEntries int) bool {
	sh.pruneLocked(cutoff)
	if sh.entries != nil {
		if exp, exists := sh.entries[key]; exists {
			if exp >= cutoff {
				return false
			}
			delete(sh.entries, key)
		}
	} else {
		sh.entries = make(map[[16]byte]int64, 64)
	}
	if maxEntries > 0 {
		for len(sh.entries) >= maxEntries {
			if !sh.popOldestLocked() {
				for k := range sh.entries {
					delete(sh.entries, k)
					break
				}
			}
		}
	}
	sh.entries[key] = nowNano
	sh.pushRingLocked(key, nowNano, maxEntries)
	return true
}

func (sh *nonceShard) pushRingLocked(key [16]byte, nowNano int64, maxEntries int) {
	if sh.ringLen == len(sh.ring) {
		newCap := len(sh.ring) * 2
		if newCap < 64 {
			newCap = 64
		}
		if maxEntries > 0 && newCap > maxEntries {
			newCap = maxEntries
		}
		if newCap <= len(sh.ring) {
			newCap = len(sh.ring) + 1
		}
		newRing := make([]nonceBinRecord, newCap)
		if sh.ringLen > 0 {
			first := len(sh.ring) - sh.ringHead
			if first > sh.ringLen {
				first = sh.ringLen
			}
			copy(newRing, sh.ring[sh.ringHead:sh.ringHead+first])
			if first < sh.ringLen {
				copy(newRing[first:], sh.ring[:sh.ringLen-first])
			}
		}
		sh.ring = newRing
		sh.ringHead = 0
	}
	tail := (sh.ringHead + sh.ringLen) % len(sh.ring)
	sh.ring[tail] = nonceBinRecord{key: key, insertedAt: nowNano}
	sh.ringLen++
}

func (sh *nonceShard) popOldestLocked() bool {
	if sh.ringLen == 0 {
		return false
	}
	rec := sh.ring[sh.ringHead]
	sh.ringHead = (sh.ringHead + 1) % len(sh.ring)
	sh.ringLen--
	if exp, ok := sh.entries[rec.key]; ok && exp == rec.insertedAt {
		delete(sh.entries, rec.key)
	}
	return true
}

func (sh *nonceShard) pruneLocked(cutoff int64) {
	for sh.ringLen > 0 {
		head := sh.ring[sh.ringHead]
		if head.insertedAt >= cutoff {
			break
		}
		sh.popOldestLocked()
	}
	if sh.ringLen == 0 && len(sh.entries) > 0 {
		for k, t := range sh.entries {
			if t < cutoff {
				delete(sh.entries, k)
			}
		}
	}
}

func (sh *nonceStrShard) addLocked(key string, nowNano, cutoff int64, maxEntries int) bool {
	sh.pruneLocked(cutoff)
	if sh.entries != nil {
		if exp, exists := sh.entries[key]; exists {
			if exp >= cutoff {
				return false
			}
			delete(sh.entries, key)
		}
	} else {
		sh.entries = make(map[string]int64, 16)
	}
	if maxEntries > 0 {
		for len(sh.entries) >= maxEntries {
			if !sh.popOldestLocked() {
				for k := range sh.entries {
					delete(sh.entries, k)
					break
				}
			}
		}
	}
	sh.entries[key] = nowNano
	sh.pushRingLocked(key, nowNano, maxEntries)
	return true
}

func (sh *nonceStrShard) pushRingLocked(key string, nowNano int64, maxEntries int) {
	if sh.ringLen == len(sh.ring) {
		newCap := len(sh.ring) * 2
		if newCap < 16 {
			newCap = 16
		}
		if maxEntries > 0 && newCap > maxEntries {
			newCap = maxEntries
		}
		if newCap <= len(sh.ring) {
			newCap = len(sh.ring) + 1
		}
		newRing := make([]nonceStrRecord, newCap)
		if sh.ringLen > 0 {
			first := len(sh.ring) - sh.ringHead
			if first > sh.ringLen {
				first = sh.ringLen
			}
			copy(newRing, sh.ring[sh.ringHead:sh.ringHead+first])
			if first < sh.ringLen {
				copy(newRing[first:], sh.ring[:sh.ringLen-first])
			}
		}
		sh.ring = newRing
		sh.ringHead = 0
	}
	tail := (sh.ringHead + sh.ringLen) % len(sh.ring)
	sh.ring[tail] = nonceStrRecord{key: key, insertedAt: nowNano}
	sh.ringLen++
}

func (sh *nonceStrShard) popOldestLocked() bool {
	if sh.ringLen == 0 {
		return false
	}
	rec := sh.ring[sh.ringHead]
	sh.ring[sh.ringHead] = nonceStrRecord{}
	sh.ringHead = (sh.ringHead + 1) % len(sh.ring)
	sh.ringLen--
	if exp, ok := sh.entries[rec.key]; ok && exp == rec.insertedAt {
		delete(sh.entries, rec.key)
	}
	return true
}

func (sh *nonceStrShard) pruneLocked(cutoff int64) {
	for sh.ringLen > 0 {
		head := sh.ring[sh.ringHead]
		if head.insertedAt >= cutoff {
			break
		}
		sh.popOldestLocked()
	}
	if sh.ringLen == 0 && len(sh.entries) > 0 {
		for k, t := range sh.entries {
			if t < cutoff {
				delete(sh.entries, k)
			}
		}
	}
}

func (nc *NonceCache) evictLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-nc.stopCh:
			return
		case <-ticker.C:
			cutoff := time.Now().UnixNano() - nc.ttl.Nanoseconds()
			for i := range nc.shards {
				sh := &nc.shards[i]
				sh.mu.Lock()
				if sh.ringLen > 0 || len(sh.entries) > 0 {
					sh.pruneLocked(cutoff)
				}
				sh.mu.Unlock()
			}
			nc.fallback.mu.Lock()
			if nc.fallback.ringLen > 0 || len(nc.fallback.entries) > 0 {
				nc.fallback.pruneLocked(cutoff)
			}
			nc.fallback.mu.Unlock()
		}
	}
}

// StripAuthHeaders removes fronting-only request metadata before a decoy proxy
// forwards unauthenticated traffic to a normal origin.
func StripAuthHeaders(h http.Header) {
	delete(h, headerVersion)
	delete(h, headerDate)
	delete(h, headerNonce)
	delete(h, headerAuth)
	delete(h, HeaderSessionID)
	delete(h, HeaderStreamSeq)
}

var staticAuthVersionSlice = []string{authVersion}

func firstHeader(h http.Header, canonicalKey string) string {
	if h == nil {
		return ""
	}
	if v := h[canonicalKey]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// SignRequestWithRawNonce adds HTTP-layer authentication headers using a
// caller-supplied 128-bit cryptographic nonce. When HeaderSessionID or
// HeaderStreamSeq are set on req, they are bound into the HMAC signature.
func SignRequestWithRawNonce(req *http.Request, key [32]byte, now time.Time, rawNonce [16]byte) {
	var nonceHex [32]byte
	hex.Encode(nonceHex[:], rawNonce[:])
	nonce := string(nonceHex[:])

	var tsBuf [24]byte
	ts := string(strconv.AppendInt(tsBuf[:0], now.Unix(), 10))

	sid := firstHeader(req.Header, HeaderSessionID)
	seq := firstHeader(req.Header, HeaderStreamSeq)

	req.Header[headerVersion] = staticAuthVersionSlice
	req.Header[headerDate] = []string{ts}
	req.Header[headerNonce] = []string{nonce}
	sum := computeMACBytes(req.Method, req.URL.EscapedPath(), req.Host, ts, nonce, sid, seq, key)
	const sigPrefix = "HMAC-SHA256 Signature="
	var authBuf [len(sigPrefix) + sha256.Size*2]byte
	copy(authBuf[:], sigPrefix)
	hex.Encode(authBuf[len(sigPrefix):], sum[:])
	req.Header[headerAuth] = []string{string(authBuf[:])}
}

// SignRequest adds HTTP-layer authentication headers. When HeaderSessionID or
// HeaderStreamSeq are set on req, they are bound into the HMAC signature.
func SignRequest(req *http.Request, key [32]byte, now time.Time) error {
	var rawNonce [16]byte
	if _, err := rand.Read(rawNonce[:]); err != nil {
		return fmt.Errorf("fronting: nonce: %w", err)
	}
	SignRequestWithRawNonce(req, key, now, rawNonce)
	return nil
}

// VerifyRequest checks HTTP-layer auth against any accepted key.
// The nonceCache parameter prevents replay attacks; pass nil to skip replay protection.
func VerifyRequest(req *http.Request, keys [][32]byte, now time.Time, skew time.Duration, nonceCache ...*NonceCache) bool {
	if len(keys) == 0 {
		return false
	}
	if firstHeader(req.Header, headerVersion) != authVersion {
		return false
	}
	ts := firstHeader(req.Header, headerDate)
	nonce := firstHeader(req.Header, headerNonce)
	authHeader := firstHeader(req.Header, headerAuth)
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

	sid := firstHeader(req.Header, HeaderSessionID)
	seq := firstHeader(req.Header, HeaderStreamSeq)
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

func decodeHex16(s string, dst *[16]byte) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < 16; i++ {
		hi, ok1 := decodeHexNibble(s[i*2])
		lo, ok2 := decodeHexNibble(s[i*2+1])
		if !ok1 || !ok2 {
			return false
		}
		dst[i] = (hi << 4) | lo
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
