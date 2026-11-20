package shaping

import (
	"bytes"
	"encoding/binary"
	"errors"
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
		_, _ = client.Write(msg)
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
		_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
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
		_, _ = client.Write(payload)
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
		_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
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

func TestIdleSilentNoContinuousDummyFlood(t *testing.T) {
	rawA, rawB := tcpPipe(t)
	cfg := Config{
		FrameSize:            64,
		Interval:             1 * time.Millisecond,
		BurstSlots:           4,
		IdleChaffMinInterval: 5 * time.Second,
		IdleChaffMaxInterval: 10 * time.Second,
	}
	shaped := Wrap(rawA, cfg)
	defer shaped.Close()
	defer rawB.Close()

	// Before IdleChaffMinInterval elapses, an idle connection must emit zero bytes.
	buf := make([]byte, 64)
	_ = rawB.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	n, err := rawB.Read(buf)
	if n > 0 {
		t.Fatalf("idle connection emitted %d unexpected bytes before chaff interval", n)
	}
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("expected read timeout while idle, got %v", err)
	}
}

func TestSparseRandomChaffOnIdle(t *testing.T) {
	rawA, rawB := tcpPipe(t)
	cfg := Config{
		FrameSize:              128,
		Interval:               1 * time.Millisecond,
		BurstSlots:             4,
		IdleChaffMinInterval:   15 * time.Millisecond,
		IdleChaffMaxInterval:   30 * time.Millisecond,
		IdleChaffMinSize:       16,
		IdleChaffMaxSize:       96,
		IdleChaffMaxBurst:      3,
		IdleChaffBurstMinDelay: 1 * time.Millisecond,
		IdleChaffBurstMaxDelay: 3 * time.Millisecond,
	}
	shaped := Wrap(rawA, cfg)
	defer shaped.Close()
	defer rawB.Close()

	// Inspect raw wire frames produced while idle.
	_ = rawB.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [outerHeaderLen]byte
	seenSizes := make(map[int]struct{})
	var nonZeroPayloads int
	for i := 0; i < 4; i++ {
		if _, err := io.ReadFull(rawB, hdr[:]); err != nil {
			t.Fatalf("read chaff header %d: %v", i, err)
		}
		if hdr[0] != frameTypeDummy {
			t.Fatalf("chaff frame %d type = %d, want %d", i, hdr[0], frameTypeDummy)
		}
		bodyLen := int(binary.BigEndian.Uint16(hdr[1:3]))
		if bodyLen < cfg.IdleChaffMinSize || bodyLen > cfg.IdleChaffMaxSize {
			t.Fatalf("chaff body len = %d, want [%d, %d]", bodyLen, cfg.IdleChaffMinSize, cfg.IdleChaffMaxSize)
		}
		seenSizes[bodyLen] = struct{}{}
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(rawB, body); err != nil {
			t.Fatalf("read chaff body %d: %v", i, err)
		}
		for _, b := range body {
			if b != 0 {
				nonZeroPayloads++
				break
			}
		}
	}
	if nonZeroPayloads == 0 {
		t.Fatal("chaff frames contained all zeros instead of random noise")
	}
	if len(seenSizes) < 2 {
		t.Fatalf("chaff frames had constant size: %v", seenSizes)
	}
}

func TestSparseChaffStrippedByPeerRead(t *testing.T) {
	rawClient, rawServer := tcpPipe(t)
	clientCfg := Config{
		FrameSize:              128,
		Interval:               1 * time.Millisecond,
		BurstSlots:             4,
		IdleChaffMinInterval:   10 * time.Millisecond,
		IdleChaffMaxInterval:   20 * time.Millisecond,
		IdleChaffMinSize:       24,
		IdleChaffMaxSize:       80,
		IdleChaffMaxBurst:      2,
		IdleChaffBurstMinDelay: 1 * time.Millisecond,
		IdleChaffBurstMaxDelay: 2 * time.Millisecond,
	}
	client := Wrap(rawClient, clientCfg)
	server := Wrap(rawServer, Config{FrameSize: 128, Interval: time.Millisecond, BurstSlots: 4})
	defer client.Close()
	defer server.Close()

	// Let idle chaff run first, then send real data.
	time.Sleep(60 * time.Millisecond)

	msg := []byte("real payload after messy chaff")
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("write after chaff: %v", err)
	}

	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("read after chaff: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}
}

func TestWriteBufferBackpressureBounded(t *testing.T) {
	// net.Pipe is unbuffered, so writeLoop blocks inside c.inner.Write until
	// the peer reads; this deterministically saturates writeBuf at MaxBufferSize.
	rawA, rawB := net.Pipe()
	defer rawB.Close()

	cfg := Config{
		FrameSize:     64,
		Interval:      1 * time.Millisecond,
		BurstSlots:    1,
		MaxBufferSize: 128,
	}
	shaped := Wrap(rawA, cfg)
	defer shaped.Close()

	writeDone := make(chan error, 1)
	bigPayload := bytes.Repeat([]byte("A"), 4096)
	go func() {
		_, err := shaped.Write(bigPayload)
		writeDone <- err
	}()

	// Write must block due to MaxBufferSize backpressure while peer isn't reading.
	select {
	case err := <-writeDone:
		t.Fatalf("Write returned %v before peer drained, expected backpressure block", err)
	case <-time.After(40 * time.Millisecond):
	}

	// Start draining from peer: Write must unblock and finish cleanly.
	peer := Wrap(rawB, cfg)
	defer peer.Close()

	readDone := make(chan error, 1)
	go func() {
		got := make([]byte, len(bigPayload))
		_, err := io.ReadFull(peer, got)
		if err == nil && !bytes.Equal(got, bigPayload) {
			err = errors.New("payload mismatch")
		}
		readDone <- err
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("Write after drain failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Write remained blocked after peer started draining")
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("peer read failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer read timed out")
	}
}

func TestCloseNoDeadlockOnInnerWriteError(t *testing.T) {
	rawA, rawB := tcpPipe(t)
	shaped := Wrap(rawA, Config{
		FrameSize:  64,
		Interval:   1 * time.Millisecond,
		BurstSlots: 2,
	})

	// Close peer and local raw socket so writeLoop's inner.Write fails.
	_ = rawB.Close()
	_ = rawA.Close()

	_, _ = shaped.Write([]byte("trigger inner write error"))

	done := make(chan error, 1)
	go func() {
		done <- shaped.Close()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Conn.Close deadlocked after inner write error")
	}
}

func TestShapingRingWrapAroundLargeTransfer(t *testing.T) {
	rawA, rawB := tcpPipe(t)
	cfg := Config{
		FrameSize:            256,
		Interval:             1 * time.Millisecond,
		BurstSlots:           8,
		MaxBufferSize:        4096,
		IdleChaffMaxInterval: 0,
	}
	client := Wrap(rawA, cfg)
	server := Wrap(rawB, cfg)
	defer client.Close()
	defer server.Close()

	// Transfer 256 KiB through a tiny 4 KiB ring buffer so the circular buffer
	// wraps around more than 60 times and coalesces bursts cleanly.
	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte(i*31 + i>>9)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := client.Write(payload)
		errCh <- err
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("ReadFull shaped 256 KiB: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Write shaped 256 KiB: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("ring wrap payload mismatch at byte %d", firstDiff(got, payload))
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
