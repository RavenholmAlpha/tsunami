package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestPrivateTargetPolicyCoversReservedRanges(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"127.0.0.1:80", true},
		{"10.0.0.1:80", true},
		{"172.16.0.1:80", true},
		{"192.168.1.1:80", true},
		{"169.254.169.254:80", true}, // cloud metadata
		{"100.100.100.200:80", true}, // Alibaba Cloud metadata / CGNAT
		{"100.64.0.1:80", true},      // CGNAT
		{"198.18.0.1:80", true},      // benchmarking
		{"224.0.0.1:80", true},       // multicast
		{"255.255.255.255:80", true},
		{"[::]:80", true},
		{"[fe80::1%25eth0]:80", true},
		{"[::1]:80", true},
		{"1.1.1.1:53", false},
		{"8.8.8.8:53", false},
	}
	for _, tc := range cases {
		if got := isPrivateTarget(tc.target); got != tc.want {
			t.Errorf("isPrivateTarget(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

func TestSplitTargetRejectsZoneAndBadPort(t *testing.T) {
	for _, target := range []string{"[fe80::1%eth0]:80", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1"} {
		if _, _, err := splitTarget(target); err == nil {
			t.Errorf("splitTarget(%q) accepted an invalid target", target)
		}
	}
}

func TestDialTargetBlocksPrivateLiteral(t *testing.T) {
	s := &Server{config: Config{}}
	if _, err := s.dialTarget(context.Background(), "127.0.0.1:80"); !errors.Is(err, errBlockedTarget) {
		t.Fatalf("dialTarget(127.0.0.1:80) = %v, want errBlockedTarget", err)
	}
	// AllowAll bypasses the policy (connection refusal is fine; it must not be
	// the policy error).
	allowAll := &Server{config: Config{AllowAll: true}}
	if _, err := allowAll.dialTarget(context.Background(), "127.0.0.1:1"); errors.Is(err, errBlockedTarget) {
		t.Fatalf("allow-all dial was blocked by policy: %v", err)
	}
}

func TestDialHappyEyeballsFastFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	// RFC5737 TEST-NET-1 (192.0.2.1) either fails fast or drops packets; stagger
	// 50ms ensures working 127.0.0.1 wins well under 1s instead of waiting 10s.
	addrs := interleaveAddresses([]netip.Addr{
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("127.0.0.1"),
	})
	start := time.Now()
	conn, err := dialHappyEyeballs(context.Background(), addrs, port, 50*time.Millisecond, 3*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dialHappyEyeballs failed: %v", err)
	}
	_ = conn.Close()
	select {
	case sc := <-accepted:
		_ = sc.Close()
	case <-time.After(time.Second):
		t.Fatal("listener did not accept connection")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Happy Eyeballs fallback took too long: %v", elapsed)
	}
}
