package control

import (
	"context"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// Direction identifies traffic direction from the user's perspective.
type Direction string

const (
	DirectionUpload   Direction = "upload"
	DirectionDownload Direction = "download"
)

// Limiter applies runtime speed limits.
type Limiter interface {
	Wait(ctx context.Context, user *protocol.UserInfo, direction Direction, n int) error
}

// UsageRecorder records runtime traffic usage.
type UsageRecorder interface {
	Record(user *protocol.UserInfo, direction Direction, n int)
}

// TrafficPolicy wires usage accounting and limiting into relay paths.
type TrafficPolicy struct {
	Limiter Limiter
	Usage   UsageRecorder
	Context context.Context
}

// WrapReader wraps a reader with accounting and limit enforcement.
func (p TrafficPolicy) WrapReader(r io.Reader, user *protocol.UserInfo, direction Direction) io.Reader {
	if r == nil || (p.Limiter == nil && p.Usage == nil) {
		return r
	}
	return &trafficReader{
		reader:    r,
		policy:    p,
		user:      user,
		direction: direction,
	}
}

// WrapReadWriteCloser wraps a stream where reads are user uploads and writes
// are user downloads.
func (p TrafficPolicy) WrapReadWriteCloser(rwc io.ReadWriteCloser, user *protocol.UserInfo) io.ReadWriteCloser {
	if rwc == nil || (p.Limiter == nil && p.Usage == nil) {
		return rwc
	}
	return &trafficReadWriteCloser{
		rwc:    rwc,
		policy: p,
		user:   user,
	}
}

type trafficReader struct {
	reader    io.Reader
	policy    TrafficPolicy
	user      *protocol.UserInfo
	direction Direction
}

func (r *trafficReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		if waitErr := r.policy.wait(r.user, r.direction, n); waitErr != nil {
			return n, waitErr
		}
		r.policy.record(r.user, r.direction, n)
	}
	return n, err
}

type trafficReadWriteCloser struct {
	rwc    io.ReadWriteCloser
	policy TrafficPolicy
	user   *protocol.UserInfo
}

func (rw *trafficReadWriteCloser) Read(p []byte) (int, error) {
	n, err := rw.rwc.Read(p)
	if n > 0 {
		if waitErr := rw.policy.wait(rw.user, DirectionUpload, n); waitErr != nil {
			return n, waitErr
		}
		rw.policy.record(rw.user, DirectionUpload, n)
	}
	return n, err
}

func (rw *trafficReadWriteCloser) Write(p []byte) (int, error) {
	if len(p) > 0 {
		if err := rw.policy.wait(rw.user, DirectionDownload, len(p)); err != nil {
			return 0, err
		}
	}
	n, err := rw.rwc.Write(p)
	if n > 0 {
		rw.policy.record(rw.user, DirectionDownload, n)
	}
	return n, err
}

func (rw *trafficReadWriteCloser) Close() error {
	return rw.rwc.Close()
}

func (p TrafficPolicy) wait(user *protocol.UserInfo, direction Direction, n int) error {
	if p.Limiter == nil || n <= 0 {
		return nil
	}
	ctx := p.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return p.Limiter.Wait(ctx, user, direction, n)
}

func (p TrafficPolicy) record(user *protocol.UserInfo, direction Direction, n int) {
	if p.Usage == nil || n <= 0 {
		return
	}
	p.Usage.Record(user, direction, n)
}

// UsageDelta is one user's usage over a reporting interval.
type UsageDelta struct {
	UserID        string
	UploadBytes   int64
	DownloadBytes int64
}

// UsageTracker accumulates per-user usage deltas for panel reporting.
type UsageTracker struct {
	mu     sync.Mutex
	deltas map[string]*UsageDelta
}

// NewUsageTracker creates an empty usage tracker.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{deltas: make(map[string]*UsageDelta)}
}

// Record records usage for one user.
func (t *UsageTracker) Record(user *protocol.UserInfo, direction Direction, n int) {
	if t == nil || user == nil || n <= 0 {
		return
	}

	key := user.Identity()
	t.mu.Lock()
	defer t.mu.Unlock()

	delta := t.deltas[key]
	if delta == nil {
		delta = &UsageDelta{UserID: key}
		t.deltas[key] = delta
	}

	switch direction {
	case DirectionUpload:
		delta.UploadBytes += int64(n)
	case DirectionDownload:
		delta.DownloadBytes += int64(n)
	}
}

// Snapshot returns current deltas without resetting them.
func (t *UsageTracker) Snapshot() []UsageDelta {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneUsageDeltas(t.deltas)
}

// SnapshotAndReset returns current deltas and clears the tracker.
func (t *UsageTracker) SnapshotAndReset() []UsageDelta {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	deltas := cloneUsageDeltas(t.deltas)
	t.deltas = make(map[string]*UsageDelta)
	return deltas
}

func cloneUsageDeltas(source map[string]*UsageDelta) []UsageDelta {
	deltas := make([]UsageDelta, 0, len(source))
	for _, delta := range source {
		if delta == nil {
			continue
		}
		deltas = append(deltas, *delta)
	}
	sort.Slice(deltas, func(i, j int) bool {
		return deltas[i].UserID < deltas[j].UserID
	})
	return deltas
}

type limiterBucket struct {
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// UserLimiter is a simple per-user global token-bucket limiter.
type UserLimiter struct {
	mu      sync.Mutex
	buckets map[string]*limiterBucket
}

// NewUserLimiter creates a per-user limiter.
func NewUserLimiter() *UserLimiter {
	return &UserLimiter{buckets: make(map[string]*limiterBucket)}
}

// Wait blocks until n bytes are allowed for the user.
func (l *UserLimiter) Wait(ctx context.Context, user *protocol.UserInfo, direction Direction, n int) error {
	if l == nil || user == nil || n <= 0 {
		return nil
	}

	rate := userLimitBps(user)
	if rate <= 0 {
		return nil
	}

	if ctx == nil {
		ctx = context.Background()
	}

	wait := l.reserve(user.Identity(), rate, n)
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (l *UserLimiter) reserve(key string, rateBps int64, n int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	rate := float64(rateBps)
	burst := rate
	if burst < 64*1024 {
		burst = 64 * 1024
	}

	bucket := l.buckets[key]
	if bucket == nil || bucket.rate != rate {
		bucket = &limiterBucket{
			rate:   rate,
			burst:  burst,
			tokens: burst,
			last:   now,
		}
		l.buckets[key] = bucket
	}

	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens += elapsed * bucket.rate
		if bucket.tokens > bucket.burst {
			bucket.tokens = bucket.burst
		}
		bucket.last = now
	}

	bucket.tokens -= float64(n)
	if bucket.tokens >= 0 {
		return 0
	}

	seconds := -bucket.tokens / bucket.rate
	return time.Duration(seconds * float64(time.Second))
}

func userLimitBps(user *protocol.UserInfo) int64 {
	if user.SpeedLimitBps > 0 {
		return user.SpeedLimitBps
	}
	if user.Bandwidth > 0 {
		return int64(user.Bandwidth) * 1000 * 1000 / 8
	}
	return 0
}
