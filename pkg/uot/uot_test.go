package uot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type mockStream struct {
	io.Reader
	io.Writer
}

func (m *mockStream) Close() error { return nil }

func TestRelay_resolve(t *testing.T) {
	// Construct a UoT stream payload:
	// Packet 1: ATYP=IPv4, IP=127.0.0.1, Port=80, DataLen=5, Data="hello"
	// Packet 2: ATYP=IPv4, IP=10.0.0.1, Port=80, DataLen=5, Data="world"
	var buf bytes.Buffer

	// Packet 1 (127.0.0.1:80)
	buf.WriteByte(protocol.AtypIPv4)
	buf.Write([]byte{127, 0, 0, 1})
	buf.Write([]byte{0, 80}) // port 80
	buf.Write([]byte{0, 5})  // len 5
	buf.WriteString("hello")

	// Packet 2 (10.0.0.1:80)
	buf.WriteByte(protocol.AtypIPv4)
	buf.Write([]byte{10, 0, 0, 1})
	buf.Write([]byte{0, 80}) // port 80
	buf.Write([]byte{0, 5})  // len 5
	buf.WriteString("world")

	stream := &mockStream{Reader: &buf, Writer: io.Discard}

	allowedTargets := make(map[string]bool)
	var mu sync.Mutex

	resolve := func(target string) (*net.UDPAddr, error) {
		mu.Lock()
		defer mu.Unlock()
		// Only allow 10.0.0.1:80.
		if target == "10.0.0.1:80" {
			allowedTargets[target] = true
			return &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 80}, nil
		}
		return nil, errors.New("blocked by policy")
	}

	relay := NewRelay(stream, resolve)

	// Create UDP socket to satisfy Run / streamToUDP (but we just want to run streamToUDP)
	// Let's listen on UDP port 0 to get a local listener
	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay.udpConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.udpConn.Close()

	// Run streamToUDP in a separate goroutine
	done := make(chan struct{})
	go func() {
		relay.streamToUDP()
		close(done)
	}()

	// Wait up to 1 second for the loop to parse the two packets and exit (since stream EOF is reached)
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for streamToUDP to exit")
	}

	mu.Lock()
	defer mu.Unlock()
	if allowedTargets["127.0.0.1:80"] {
		t.Error("127.0.0.1:80 should have been blocked")
	}
	if !allowedTargets["10.0.0.1:80"] {
		t.Error("10.0.0.1:80 should have been allowed")
	}
}

func TestRelayInstantCloseAndResolveCaching(t *testing.T) {
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echoConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echoConn.WriteToUDP(buf[:n], addr)
		}
	}()

	echoPort := echoConn.LocalAddr().(*net.UDPAddr).Port
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()

	var resolveCalls atomic.Int32
	relay := NewRelay(&duplexStreamForTest{r: c2sR, w: s2cW}, func(target string) (*net.UDPAddr, error) {
		resolveCalls.Add(1)
		return net.ResolveUDPAddr("udp", target)
	})

	runDone := make(chan error, 1)
	go func() {
		runDone <- relay.Run()
	}()

	// Send 5 packets to the same target; resolve should be called only once.
	var pkt bytes.Buffer
	pkt.WriteByte(protocol.AtypIPv4)
	pkt.Write([]byte{127, 0, 0, 1})
	_ = binary.Write(&pkt, binary.BigEndian, uint16(echoPort))
	_ = binary.Write(&pkt, binary.BigEndian, uint16(4))
	pkt.WriteString("ping")

	resp := make([]byte, 9+4)
	for i := 0; i < 5; i++ {
		if _, err := c2sW.Write(pkt.Bytes()); err != nil {
			t.Fatalf("write pkt %d: %v", i, err)
		}
		if _, err := io.ReadFull(s2cR, resp); err != nil {
			t.Fatalf("read resp %d: %v", i, err)
		}
		if string(resp[9:]) != "ping" {
			t.Fatalf("reply = %q, want ping", resp[9:])
		}
	}
	if got := resolveCalls.Load(); got != 1 {
		t.Fatalf("resolve calls = %d, want 1 (cached)", got)
	}

	// Closing client uplink must stop Run() immediately rather than waiting 120s.
	_ = c2sW.Close()
	_ = s2cR.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Relay.Run returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Relay.Run did not exit promptly after stream EOF (120s leak)")
	}
}

type duplexStreamForTest struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (d *duplexStreamForTest) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d *duplexStreamForTest) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d *duplexStreamForTest) Close() error {
	_ = d.r.Close()
	return d.w.Close()
}

func TestRelayShortNATTimeoutStaysAliveUnderTraffic(t *testing.T) {
	echoAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	echoConn, err := net.ListenUDP("udp", echoAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echoConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echoConn.WriteToUDP(buf[:n], addr)
		}
	}()

	echoPort := echoConn.LocalAddr().(*net.UDPAddr).Port
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	relay := NewRelay(&duplexStreamForTest{r: c2sR, w: s2cW}, func(target string) (*net.UDPAddr, error) {
		return net.ResolveUDPAddr("udp", target)
	})
	relay.natTimeout = 150 * time.Millisecond

	runDone := make(chan error, 1)
	go func() {
		runDone <- relay.Run()
	}()

	var pkt bytes.Buffer
	pkt.WriteByte(protocol.AtypIPv4)
	pkt.Write([]byte{127, 0, 0, 1})
	_ = binary.Write(&pkt, binary.BigEndian, uint16(echoPort))
	_ = binary.Write(&pkt, binary.BigEndian, uint16(4))
	pkt.WriteString("pong")

	resp := make([]byte, 9+4)
	for i := 0; i < 6; i++ {
		if i > 0 {
			time.Sleep(40 * time.Millisecond)
		}
		if _, err := c2sW.Write(pkt.Bytes()); err != nil {
			t.Fatalf("write pkt %d: %v", i, err)
		}
		if _, err := io.ReadFull(s2cR, resp); err != nil {
			t.Fatalf("read resp %d: %v", i, err)
		}
		if string(resp[9:]) != "pong" {
			t.Fatalf("reply %d = %q, want pong", i, resp[9:])
		}
	}

	_ = c2sW.Close()
	_ = s2cR.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Relay.Run returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Relay.Run did not exit promptly after EOF")
	}
}
