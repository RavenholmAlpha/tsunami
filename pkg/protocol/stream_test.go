package protocol

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func tcpPipeProtocol(t *testing.T) (net.Conn, net.Conn) {
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

func startSessionPair(t *testing.T, clientWindow, serverWindow int) (clientSess, serverSess *Session, serverStreams <-chan *Stream) {
	t.Helper()
	cConn, sConn := tcpPipeProtocol(t)
	clientSess = NewSession(cConn, 1)
	serverSess = NewSession(sConn, 1)
	if clientWindow > 0 && serverWindow > 0 {
		clientSess.EnableFlowControl(clientWindow, serverWindow)
		serverSess.EnableFlowControl(serverWindow, clientWindow)
	}

	streamsCh := make(chan *Stream, 16)
	serverSess.SetOnStreamOpen(func(s *Stream) {
		streamsCh <- s
	})

	go func() { _ = clientSess.RunEventLoop() }()
	go func() { _ = serverSess.RunEventLoop() }()

	t.Cleanup(func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	})

	return clientSess, serverSess, streamsCh
}

func TestStreamDeliverDataNonBlockingAndOverflow(t *testing.T) {
	stream := newStream(1, NewSession(&testReadWriteCloser{}, 1))
	stream.setMaxRecvBytes(8)

	// Delivering multiple frames within maxRecvBytes must not block.
	for i := 0; i < 8; i++ {
		if err := stream.deliverData([]byte{byte(i)}); err != nil {
			t.Fatalf("deliver byte %d: %v", i, err)
		}
	}

	// Exceeding maxRecvBytes must fail immediately with ErrStreamRecvOverflow rather than blocking.
	done := make(chan error, 1)
	go func() {
		done <- stream.deliverData([]byte{99})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamRecvOverflow) {
			t.Fatalf("overflow error = %v, want %v", err, ErrStreamRecvOverflow)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("deliverData blocked instead of returning ErrStreamRecvOverflow")
	}

	// Drain buffered data cleanly.
	buf := make([]byte, 8)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("drain buffered bytes: %v", err)
	}
	for i := 0; i < 8; i++ {
		if buf[i] != byte(i) {
			t.Fatalf("buf[%d] = %d, want %d", i, buf[i], i)
		}
	}
}

func TestStreamReadDrainsBufferedDataBeforeEOF(t *testing.T) {
	stream := newStream(1, NewSession(&testReadWriteCloser{}, 1))
	if err := stream.deliverData([]byte("tail-")); err != nil {
		t.Fatalf("deliver 1: %v", err)
	}
	if err := stream.deliverData([]byte("payload")); err != nil {
		t.Fatalf("deliver 2: %v", err)
	}
	stream.closeByRemote(nil)

	all, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll after FIN: %v", err)
	}
	if string(all) != "tail-payload" {
		t.Fatalf("ReadAll = %q, want %q", string(all), "tail-payload")
	}
}

func TestStreamFlowControlBackpressureAndResume(t *testing.T) {
	const win = 4096
	clientSess, _, serverStreams := startSessionPair(t, win, win)

	cStream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer cStream.Close()

	var sStream *Stream
	select {
	case sStream = <-serverStreams:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server stream")
	}
	defer sStream.Close()

	payload := bytes.Repeat([]byte("W"), 64*1024)
	writeDone := make(chan error, 1)
	go func() {
		_, err := cStream.Write(payload)
		writeDone <- err
	}()

	// Writer should pause once the 4096-byte send window is exhausted.
	select {
	case err := <-writeDone:
		t.Fatalf("Write finished (%v) without waiting for WINDOW_UPDATE", err)
	case <-time.After(80 * time.Millisecond):
	}

	// Reading on server drains recvQueue and emits CmdWindowUpdate, unblocking writer.
	received := make([]byte, len(payload))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(sStream, received)
		readDone <- err
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("Write failed after reader resumed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Write did not resume after reader sent WINDOW_UPDATE")
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("ReadFull failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadFull timed out")
	}

	if !bytes.Equal(received, payload) {
		t.Fatal("received payload mismatch under sliding window flow control")
	}
}

func TestSessionHeadOfLineIsolation(t *testing.T) {
	const win = 8192
	clientSess, _, serverStreams := startSessionPair(t, win, win)

	// Stream 1: server never reads from slowServerStream while client floods 64 KiB.
	slowClientStream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("open slow stream: %v", err)
	}
	defer slowClientStream.Close()

	var slowServerStream *Stream
	select {
	case slowServerStream = <-serverStreams:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for slow server stream")
	}
	defer slowServerStream.Close()

	go func() {
		_, _ = slowClientStream.Write(bytes.Repeat([]byte("S"), 64*1024))
	}()
	time.Sleep(50 * time.Millisecond)

	// Stream 2: fast stream must still open and exchange data immediately.
	fastClientStream, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("open fast stream while slow stream blocked: %v", err)
	}
	defer fastClientStream.Close()

	var fastServerStream *Stream
	select {
	case fastServerStream = <-serverStreams:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fast server stream")
	}
	defer fastServerStream.Close()

	go func() {
		buf := make([]byte, 128)
		n, err := fastServerStream.Read(buf)
		if err == nil {
			_, _ = fastServerStream.Write(buf[:n])
		}
	}()

	msg := []byte("fast-stream-unblocked")
	if _, err := fastClientStream.Write(msg); err != nil {
		t.Fatalf("write fast stream: %v", err)
	}
	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(fastClientStream, reply); err != nil {
		t.Fatalf("read fast stream: %v", err)
	}
	if !bytes.Equal(reply, msg) {
		t.Fatalf("fast echo = %q, want %q", reply, msg)
	}

	// Heartbeat must also complete cleanly without HoL stall.
	beforePong := clientSess.LastPong()
	if err := clientSess.SendHeartbeat(); err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if clientSess.LastPong().After(beforePong) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("heartbeat pong delayed/blocked by slow stream")
}

func TestSessionOverflowIsolationWithoutFlowControl(t *testing.T) {
	clientSess, _, serverStreams := startSessionPair(t, 0, 0)

	s1Client, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("open stream 1: %v", err)
	}
	defer s1Client.Close()

	var s1Server *Stream
	select {
	case s1Server = <-serverStreams:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream 1")
	}
	s1Server.setMaxRecvBytes(1024)

	// Flood stream 1 past its 1024-byte cap without reading.
	if _, err := s1Client.Write(bytes.Repeat([]byte("X"), 4096)); err != nil {
		t.Fatalf("write overflow burst: %v", err)
	}

	// Wait briefly for server RunEventLoop to close only stream 1.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s1Server.closed.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !s1Server.closed.Load() {
		t.Fatal("overflowing stream 1 was not closed")
	}
	if clientSess.IsClosed() {
		t.Fatal("session was closed by single stream overflow")
	}

	// Stream 2 must still work on the same session!
	s2Client, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("open stream 2 after stream 1 overflow: %v", err)
	}
	defer s2Client.Close()

	var s2Server *Stream
	select {
	case s2Server = <-serverStreams:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream 2")
	}
	defer s2Server.Close()

	go func() {
		buf := make([]byte, 64)
		n, err := s2Server.Read(buf)
		if err == nil {
			_, _ = s2Server.Write(buf[:n])
		}
	}()

	msg := []byte("session-healthy")
	if _, err := s2Client.Write(msg); err != nil {
		t.Fatalf("write stream 2: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(s2Client, got); err != nil {
		t.Fatalf("read stream 2: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("stream 2 = %q, want %q", got, msg)
	}
}
