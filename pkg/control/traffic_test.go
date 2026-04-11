package control

import (
	"bytes"
	"io"
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
