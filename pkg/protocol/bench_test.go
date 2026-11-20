package protocol

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

func startBenchSessionPair(b *testing.B, fc bool) (*Session, *Session, <-chan *Stream) {
	b.Helper()
	cConn, sConn := tcpPipeBench(b)
	clientSess := NewSession(cConn, 1)
	serverSess := NewSession(sConn, 1)
	if fc {
		clientSess.EnableFlowControl(DefaultStreamWindow, DefaultStreamWindow)
		serverSess.EnableFlowControl(DefaultStreamWindow, DefaultStreamWindow)
	}

	streamsCh := make(chan *Stream, 64)
	serverSess.SetOnStreamOpen(func(s *Stream) {
		streamsCh <- s
	})

	go func() { _ = clientSess.RunEventLoop() }()
	go func() { _ = serverSess.RunEventLoop() }()

	b.Cleanup(func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	})
	return clientSess, serverSess, streamsCh
}

func BenchmarkFrameEncodeDecode32K(b *testing.B) {
	payload := bytes.Repeat([]byte("F"), 32*1024)
	frame := NewFrame(CmdPSH, 7, payload)
	var buf bytes.Buffer
	buf.Grow(64 * 1024)
	fw := NewFrameWriter(&buf)
	fr := NewFrameReader(&buf)

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := fw.WriteFrame(frame); err != nil {
			b.Fatal(err)
		}
		if _, err := fr.ReadFrame(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionStreamThroughput(b *testing.B) {
	clientSess, _, serverStreams := startBenchSessionPair(b, true)

	// Echo server streams concurrently.
	stopEcho := make(chan struct{})
	b.Cleanup(func() { close(stopEcho) })
	go func() {
		for {
			select {
			case <-stopEcho:
				return
			case s := <-serverStreams:
				go func(st *Stream) {
					defer st.Close()
					bufp := RelayBufPool.Get().(*[]byte)
					_, _ = io.CopyBuffer(st, st, *bufp)
					RelayBufPool.Put(bufp)
				}(s)
			}
		}
	}()

	cs, err := clientSess.OpenStream()
	if err != nil {
		b.Fatalf("OpenStream: %v", err)
	}
	b.Cleanup(func() { _ = cs.Close() })

	const chunkTotal = 2 << 20 // 2 MiB round-trip (4 MiB transferred)
	src := bytes.Repeat([]byte("A"), chunkTotal)
	dst := make([]byte, chunkTotal)

	b.SetBytes(int64(chunkTotal * 2))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		var writeErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			bufp := RelayBufPool.Get().(*[]byte)
			_, writeErr = io.CopyBuffer(cs, bytes.NewReader(src), *bufp)
			RelayBufPool.Put(bufp)
		}()
		if _, err := io.ReadFull(cs, dst); err != nil {
			b.Fatalf("ReadFull: %v", err)
		}
		wg.Wait()
		if writeErr != nil {
			b.Fatalf("Write: %v", writeErr)
		}
	}
	b.StopTimer()
}

func BenchmarkStreamOpenLatency(b *testing.B) {
	clientSess, _, serverStreams := startBenchSessionPair(b, true)
	clientSess.EnableFastOpen()

	stopEcho := make(chan struct{})
	b.Cleanup(func() { close(stopEcho) })
	go func() {
		for {
			select {
			case <-stopEcho:
				return
			case s := <-serverStreams:
				go func(st *Stream) {
					defer st.Close()
					var hdr [16]byte
					n, err := st.Read(hdr[:])
					if err == nil && n > 0 {
						_, _ = st.Write(hdr[:n])
					}
				}(s)
			}
		}
	}()

	ping := []byte("hello")
	var reply [5]byte

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := clientSess.OpenStream()
		if err != nil {
			b.Fatalf("OpenStream: %v", err)
		}
		if _, err := st.Write(ping); err != nil {
			b.Fatalf("Write: %v", err)
		}
		if _, err := io.ReadFull(st, reply[:]); err != nil {
			b.Fatalf("ReadFull: %v", err)
		}
		_ = st.Close()
	}
	b.StopTimer()
}
