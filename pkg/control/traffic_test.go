package control

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

func TestUsageTrackerRecordsAndResets(t *testing.T) {
	tracker := NewUsageTracker()
	user := &protocol.UserInfo{ID: "u1", Name: "alice"}

	tracker.Record(user, DirectionUpload, 10)
	tracker.Record(user, DirectionDownload, 20)

	deltas := tracker.Snapshot()
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want 1", len(deltas))
	}
	if deltas[0].UploadBytes != 10 || deltas[0].DownloadBytes != 20 {
		t.Fatalf("delta = %+v", deltas[0])
	}

	reset := tracker.SnapshotAndReset()
	if len(reset) != 1 {
		t.Fatalf("reset = %d, want 1", len(reset))
	}
	if len(tracker.Snapshot()) != 0 {
		t.Fatalf("tracker should be empty after reset")
	}
}

func TestUsageTrackerRestoreMergesDeltas(t *testing.T) {
	tracker := NewUsageTracker()
	user := &protocol.UserInfo{ID: "u1"}

	tracker.Record(user, DirectionUpload, 3)
	tracker.Restore([]UsageDelta{
		{UserID: "u1", UploadBytes: 10, DownloadBytes: 20},
		{UserID: "u2", DownloadBytes: 30},
	})

	deltas := tracker.Snapshot()
	if len(deltas) != 2 {
		t.Fatalf("deltas = %d, want 2", len(deltas))
	}
	if deltas[0] != (UsageDelta{UserID: "u1", UploadBytes: 13, DownloadBytes: 20}) {
		t.Fatalf("u1 delta = %+v", deltas[0])
	}
	if deltas[1] != (UsageDelta{UserID: "u2", DownloadBytes: 30}) {
		t.Fatalf("u2 delta = %+v", deltas[1])
	}
}

func TestTrafficPolicyWrapReader(t *testing.T) {
	tracker := NewUsageTracker()
	user := &protocol.UserInfo{ID: "u1"}
	policy := TrafficPolicy{Usage: tracker}

	reader := policy.WrapReader(bytes.NewReader([]byte("hello")), user, DirectionUpload)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q", string(data))
	}

	deltas := tracker.Snapshot()
	if len(deltas) != 1 || deltas[0].UploadBytes != 5 {
		t.Fatalf("deltas = %+v", deltas)
	}
}

func TestUsageTrackerAndUserLimiterConcurrent(t *testing.T) {
	tracker := NewUsageTracker()
	limiter := NewUserLimiter()
	users := []*protocol.UserInfo{
		{ID: "u1", SpeedLimitBps: 10 << 20},
		{ID: "u2", SpeedLimitBps: 0},
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			u := users[id%len(users)]
			for i := 0; i < 100; i++ {
				if err := limiter.Wait(context.Background(), u, DirectionDownload, 64); err != nil {
					t.Errorf("Wait: %v", err)
					return
				}
				tracker.Record(u, DirectionDownload, 64)
				tracker.Record(u, DirectionUpload, 32)
			}
		}(g)
	}
	wg.Wait()

	deltas := tracker.Snapshot()
	if len(deltas) != 2 {
		t.Fatalf("deltas = %d, want 2", len(deltas))
	}
	if deltas[0].DownloadBytes != 400*64 || deltas[0].UploadBytes != 400*32 {
		t.Fatalf("u1 delta = %+v", deltas[0])
	}
	if deltas[1].DownloadBytes != 400*64 || deltas[1].UploadBytes != 400*32 {
		t.Fatalf("u2 delta = %+v", deltas[1])
	}
}

func TestUsageTrackerCachedCounterSurvivesSnapshotAndReset(t *testing.T) {
	tracker := NewUsageTracker()
	user := &protocol.UserInfo{ID: "u-stream"}
	policy := TrafficPolicy{Usage: tracker}

	pr, pw := io.Pipe()
	defer pr.Close()
	reader := policy.WrapReader(pr, user, DirectionUpload)

	go func() {
		_, _ = pw.Write([]byte("chunk1"))
		_, _ = pw.Write([]byte("chunk22"))
		_ = pw.Close()
	}()

	buf := make([]byte, 6)
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatalf("read chunk1: %v", err)
	}
	first := tracker.SnapshotAndReset()
	if len(first) != 1 || first[0].UploadBytes != 6 {
		t.Fatalf("first reset = %+v, want 6 upload bytes", first)
	}
	if rem := tracker.Snapshot(); len(rem) != 0 {
		t.Fatalf("expected empty snapshot right after reset, got %+v", rem)
	}

	buf2 := make([]byte, 7)
	if _, err := io.ReadFull(reader, buf2); err != nil {
		t.Fatalf("read chunk2: %v", err)
	}
	second := tracker.Snapshot()
	if len(second) != 1 || second[0].UploadBytes != 7 {
		t.Fatalf("second snapshot = %+v, want 7 upload bytes (cached counter must survive reset)", second)
	}
}
