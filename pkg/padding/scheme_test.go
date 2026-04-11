package padding

import (
	"testing"
)

func TestParseDefault(t *testing.T) {
	scheme := DefaultScheme()
	if scheme.Stop != 8 {
		t.Errorf("stop = %d, want 8", scheme.Stop)
	}
	if len(scheme.Rules) != 8 {
		t.Errorf("rules count = %d, want 8", len(scheme.Rules))
	}

	// Rule 0: fixed 30
	segs := scheme.GetSegments(0)
	if len(segs) != 1 || segs[0].MinSize != 30 || segs[0].MaxSize != 30 {
		t.Errorf("rule 0 unexpected: %+v", segs)
	}

	// Rule 2: has check marks
	segs = scheme.GetSegments(2)
	checkCount := 0
	for _, s := range segs {
		if s.IsCheck {
			checkCount++
		}
	}
	if checkCount != 4 {
		t.Errorf("rule 2 check count = %d, want 4", checkCount)
	}

	// Keepalive config
	if scheme.Keepalive == nil {
		t.Fatal("keepalive config should be present")
	}
	if scheme.Keepalive.IntervalMinMs != 30000 {
		t.Errorf("keepalive interval min = %d, want 30000", scheme.Keepalive.IntervalMinMs)
	}
	if scheme.Keepalive.IntervalMaxMs != 60000 {
		t.Errorf("keepalive interval max = %d, want 60000", scheme.Keepalive.IntervalMaxMs)
	}
	if scheme.Keepalive.SizeMin != 4 {
		t.Errorf("keepalive size min = %d, want 4", scheme.Keepalive.SizeMin)
	}
	if scheme.Keepalive.SizeMax != 8 {
		t.Errorf("keepalive size max = %d, want 8", scheme.Keepalive.SizeMax)
	}
}

func TestParseBeyondStop(t *testing.T) {
	scheme := DefaultScheme()
	// Packet 8 and beyond should return nil (no padding)
	if segs := scheme.GetSegments(8); segs != nil {
		t.Errorf("packet 8 should return nil segments, got %+v", segs)
	}
	if segs := scheme.GetSegments(100); segs != nil {
		t.Errorf("packet 100 should return nil segments")
	}
}

func TestParseCustomScheme(t *testing.T) {
	text := `stop=3
0=50-50
1=200-300,c,400-500
2=100-200
keepalive=10000-20000:2-4`

	scheme, err := Parse(text)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if scheme.Stop != 3 {
		t.Errorf("stop = %d, want 3", scheme.Stop)
	}

	segs := scheme.GetSegments(1)
	if len(segs) != 3 {
		t.Fatalf("rule 1 segments = %d, want 3", len(segs))
	}
	if !segs[1].IsCheck {
		t.Error("rule 1 segment 1 should be check")
	}
	if segs[2].MinSize != 400 || segs[2].MaxSize != 500 {
		t.Errorf("rule 1 segment 2 = %+v, want 400-500", segs[2])
	}

	if scheme.Keepalive == nil {
		t.Fatal("keepalive should be present")
	}
	if scheme.Keepalive.SizeMin != 2 || scheme.Keepalive.SizeMax != 4 {
		t.Errorf("keepalive size = %d-%d, want 2-4", scheme.Keepalive.SizeMin, scheme.Keepalive.SizeMax)
	}
}

func TestParseNoKeepalive(t *testing.T) {
	text := `stop=2
0=30-30
1=100-200`

	scheme, err := Parse(text)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if scheme.Keepalive != nil {
		t.Error("keepalive should be nil when not specified")
	}
}

func TestMD5Consistency(t *testing.T) {
	scheme := DefaultScheme()
	md5a := scheme.MD5()
	md5b := scheme.MD5()
	if md5a != md5b {
		t.Errorf("MD5 should be consistent: %q != %q", md5a, md5b)
	}
	if len(md5a) != 32 {
		t.Errorf("MD5 length = %d, want 32", len(md5a))
	}
}

func TestRandomInRange(t *testing.T) {
	// Same min/max should return that value
	for i := 0; i < 100; i++ {
		v := RandomInRange(42, 42)
		if v != 42 {
			t.Fatalf("RandomInRange(42,42) = %d, want 42", v)
		}
	}

	// Range should be respected
	for i := 0; i < 1000; i++ {
		v := RandomInRange(10, 20)
		if v < 10 || v > 20 {
			t.Fatalf("RandomInRange(10,20) = %d, out of range", v)
		}
	}
}

func TestParseComments(t *testing.T) {
	text := `# This is a comment
stop=2
0=30-30
# Another comment
1=100-200`

	scheme, err := Parse(text)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if scheme.Stop != 2 {
		t.Errorf("stop = %d, want 2", scheme.Stop)
	}
	if len(scheme.Rules) != 2 {
		t.Errorf("rules count = %d, want 2", len(scheme.Rules))
	}
}
