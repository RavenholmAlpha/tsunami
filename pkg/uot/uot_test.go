package uot

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type mockStream struct {
	io.Reader
	io.Writer
}

func (m *mockStream) Close() error { return nil }

func TestRelay_allowAddr(t *testing.T) {
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

	allowAddr := func(target string) bool {
		mu.Lock()
		defer mu.Unlock()
		// Only allow 10.0.0.1:80
		if target == "10.0.0.1:80" {
			allowedTargets[target] = true
			return true
		}
		return false
	}

	relay := NewRelay(stream, allowAddr)

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
