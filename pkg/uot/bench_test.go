package uot

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type duplexMockStream struct {
	io.ReadCloser
	io.WriteCloser
}

func (d *duplexMockStream) Close() error {
	_ = d.ReadCloser.Close()
	return d.WriteCloser.Close()
}

func BenchmarkUoTPacketRoundTrip(b *testing.B) {
	// Loopback UDP echo server
	echoAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	echoConn, err := net.ListenUDP("udp", echoAddr)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = echoConn.Close() })

	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := echoConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echoConn.WriteToUDP(buf[:n], addr)
		}
	}()

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	relayStream := &duplexMockStream{ReadCloser: c2sR, WriteCloser: s2cW}
	relay := NewRelay(relayStream, func(target string) (*net.UDPAddr, error) {
		// Force net.ResolveUDPAddr per call so resolver caching improvement is visible too.
		addr, err := net.ResolveUDPAddr("udp", target)
		if err != nil {
			return nil, err
		}
		return addr, nil
	})

	doneCh := make(chan struct{})
	go func() {
		_ = relay.Run()
		close(doneCh)
	}()
	b.Cleanup(func() {
		_ = c2sW.Close()
		_ = s2cR.Close()
		_ = relayStream.Close()
		<-doneCh
	})

	// Build a single 512-byte domain-addressed UoT packet pointing at localhost:<echoPort>
	port := echoConn.LocalAddr().(*net.UDPAddr).Port
	host := "127.0.0.1"
	payload := bytes.Repeat([]byte("U"), 512)
	var pkt bytes.Buffer
	pkt.WriteByte(protocol.AtypDomain)
	pkt.WriteByte(byte(len(host)))
	pkt.WriteString(host)
	_ = binary.Write(&pkt, binary.BigEndian, uint16(port))
	_ = binary.Write(&pkt, binary.BigEndian, uint16(len(payload)))
	pkt.Write(payload)
	reqBytes := pkt.Bytes()

	// Response frame header for IPv4 is 1+4+2+2 = 9 bytes + 512 payload = 521 bytes
	respBuf := make([]byte, 9+len(payload))

	b.SetBytes(int64(len(payload) * 2))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c2sW.Write(reqBytes); err != nil {
			b.Fatalf("write pkt: %v", err)
		}
		if _, err := io.ReadFull(s2cR, respBuf); err != nil {
			b.Fatalf("read reply: %v", err)
		}
	}
	b.StopTimer()
}
