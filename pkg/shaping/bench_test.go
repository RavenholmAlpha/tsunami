package shaping

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
)

func tcpPipeBench(b *testing.B) (net.Conn, net.Conn) {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
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
		b.Fatal(err)
	}
	wg.Wait()
	return clientConn, serverConn
}

func BenchmarkShapingThroughput(b *testing.B) {
	rawA, rawB := tcpPipeBench(b)
	cfg := DefaultConfig()
	cfg.IdleChaffMaxInterval = 0

	client := Wrap(rawA, cfg)
	server := Wrap(rawB, cfg)
	b.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	const chunkTotal = 512 << 10 // 512 KiB per iteration
	src := bytes.Repeat([]byte("P"), chunkTotal)
	dst := make([]byte, chunkTotal)

	b.SetBytes(int64(chunkTotal))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		var writeErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, writeErr = client.Write(src)
		}()
		if _, err := io.ReadFull(server, dst); err != nil {
			b.Fatalf("ReadFull: %v", err)
		}
		wg.Wait()
		if writeErr != nil {
			b.Fatalf("Write: %v", writeErr)
		}
	}
	b.StopTimer()
}
