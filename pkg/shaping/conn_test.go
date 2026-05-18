package shaping

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// tcpPipe creates a pair of TCP connections for testing (has kernel buffers, unlike net.Pipe).
func tcpPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var serverConn net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, _ = ln.Accept()
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	return clientConn, serverConn
}

func TestRoundTrip(t *testing.T) {
	cfg := Config{
		FrameSize:  64,
		Interval:   1 * time.Millisecond,
		BurstSlots: 8,
	}
	rawClient, rawServer := tcpPipe(t)
	client := Wrap(rawClient, cfg)
	server := Wrap(rawServer, cfg)
	defer client.Close()
	defer server.Close()

	msg := []byte("hello shaped world")

	go func() {
		client.Write(msg)
	}()

	buf := make([]byte, 256)
	var received []byte
	deadline := time.After(3 * time.Second)

	for len(received) < len(msg) {
		select {
		case <-deadline:
			t.Fatalf("timeout: got %d bytes, want %d", len(received), len(msg))
		default:
		}
		server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := server.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		received = append(received, buf[:n]...)
	}

	if !bytes.Equal(received, msg) {
		t.Errorf("got %q, want %q", received, msg)
	}
}

func TestLargePayload(t *testing.T) {
	cfg := Config{
		FrameSize:  128,
		Interval:   1 * time.Millisecond,
		BurstSlots: 16,
	}
	rawClient, rawServer := tcpPipe(t)
	client := Wrap(rawClient, cfg)
	server := Wrap(rawServer, cfg)
	defer client.Close()
	defer server.Close()

	payload := make([]byte, 1000)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	go func() {
		client.Write(payload)
	}()

	received := make([]byte, 0, len(payload))
	buf := make([]byte, 512)
	deadline := time.After(5 * time.Second)

	for len(received) < len(payload) {
		select {
		case <-deadline:
			t.Fatalf("timeout: got %d bytes, want %d", len(received), len(payload))
		default:
		}
		server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := server.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		received = append(received, buf[:n]...)
	}

	if !bytes.Equal(received, payload) {
		t.Errorf("payload mismatch at byte %d", firstDiff(received, payload))
	}
}

func TestDummyFramesOnIdle(t *testing.T) {
	rawA, rawB := tcpPipe(t)
	cfg := Config{
		FrameSize:  32,
		Interval:   2 * time.Millisecond,
		BurstSlots: 1,
	}
	// Only wrap one side — read raw from the other
	shaped := Wrap(rawA, cfg)
	defer shaped.Close()

	time.Sleep(20 * time.Millisecond)

	buf := make([]byte, 32*20)
	rawB.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	n, _ := io.ReadAtLeast(rawB, buf, 32)
	rawB.Close()

	if n < 32 {
		t.Fatalf("expected at least one frame (32 bytes), got %d bytes", n)
	}

	frames := n / 32
	dummyCount := 0
	for i := 0; i < frames; i++ {
		if buf[i*32] == frameTypeDummy {
			dummyCount++
		}
	}
	if dummyCount == 0 {
		t.Error("expected dummy frames during idle, got none")
	}
}

func TestFixedFrameSize(t *testing.T) {
	rawA, rawB := tcpPipe(t)
	cfg := Config{
		FrameSize:  48,
		Interval:   1 * time.Millisecond,
		BurstSlots: 4,
	}
	shaped := Wrap(rawA, cfg)

	go func() {
		shaped.Write([]byte("test data"))
		time.Sleep(10 * time.Millisecond)
		shaped.Close()
	}()

	var total int
	buf := make([]byte, 4096)
	for {
		rawB.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := rawB.Read(buf[total:])
		total += n
		if err != nil {
			break
		}
	}
	rawB.Close()

	if total == 0 {
		t.Fatal("no data received")
	}
	if total%48 != 0 {
		t.Errorf("total bytes %d is not a multiple of frame size 48", total)
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
