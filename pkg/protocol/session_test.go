package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

type testReadWriteCloser struct {
	bytes.Buffer
}

func TestSessionSYNACKResolvesOpenStream(t *testing.T) {
	conn := &testReadWriteCloser{}
	session := NewSession(conn, 1)
	stream := newStream(1, session)
	session.streams[stream.id] = stream

	if err := session.handleFrame(NewFrame(CmdSYNACK, stream.id, nil)); err != nil {
		t.Fatalf("handle SYNACK: %v", err)
	}
	if err := stream.waitForOpen(time.Second); err != nil {
		t.Fatalf("wait for successful SYNACK: %v", err)
	}
}

func TestSessionSYNACKRejectsOpenStream(t *testing.T) {
	conn := &testReadWriteCloser{}
	session := NewSession(conn, 1)
	stream := newStream(1, session)
	session.streams[stream.id] = stream

	if err := session.handleFrame(NewFrame(CmdSYNACK, stream.id, []byte("policy denied"))); err != nil {
		t.Fatalf("handle rejected SYNACK: %v", err)
	}
	if err := stream.waitForOpen(time.Second); !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("wait error = %v, want ErrStreamRejected", err)
	}
}

func (c *testReadWriteCloser) Close() error {
	return nil
}

func TestSessionUpdatePaddingSchemeCallback(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	payload := []byte("stop=1\n0=50-50")

	var got []byte
	session.SetOnPaddingSchemeUpdate(func(data []byte) error {
		got = append([]byte(nil), data...)
		return nil
	})

	if err := session.handleFrame(NewFrame(CmdUpdatePaddingScheme, 0, payload)); err != nil {
		t.Fatalf("handle update padding frame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("callback data = %q, want %q", got, payload)
	}
}

func TestSessionUpdatePaddingSchemeCallbackError(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	wantErr := errors.New("bad padding update")

	session.SetOnPaddingSchemeUpdate(func(data []byte) error {
		return wantErr
	})

	err := session.handleFrame(NewFrame(CmdUpdatePaddingScheme, 0, []byte("bad")))
	if !errors.Is(err, wantErr) {
		t.Fatalf("handle update padding error = %v, want %v", err, wantErr)
	}
}

func TestStreamWriteBatchesPaddingFrames(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	stream := newStream(1, session)

	var calls int
	var frameCount int
	session.SetPaddingWriteFn(func(frames []*Frame) error {
		calls++
		frameCount = len(frames)
		return nil
	})

	data := make([]byte, MaxFrameDataLen+1)
	n, err := stream.Write(data)
	if err != nil {
		t.Fatalf("stream write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("write n = %d, want %d", n, len(data))
	}
	if calls != 1 {
		t.Fatalf("padding write calls = %d, want 1", calls)
	}
	if frameCount != 2 {
		t.Fatalf("batched frame count = %d, want 2", frameCount)
	}
}

func TestSessionOnStreamClosedHook(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	stream := newStream(7, session)
	session.streams[stream.id] = stream

	closed := make(chan uint32, 1)
	session.SetOnStreamClosed(func(id uint32) {
		closed <- id
	})

	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	select {
	case id := <-closed:
		if id != 7 {
			t.Fatalf("closed stream id = %d, want 7", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onStreamClosed was not called for a locally closed stream")
	}
	if got := session.ActiveStreamCount(); got != 0 {
		t.Fatalf("active streams = %d, want 0", got)
	}
}

func TestSessionOnStreamClosedHookOnRemoteFIN(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	stream := newStream(9, session)
	session.streams[9] = stream

	closed := make(chan uint32, 1)
	session.SetOnStreamClosed(func(id uint32) {
		closed <- id
	})

	if err := session.handleFrame(NewFrame(CmdFIN, 9, nil)); err != nil {
		t.Fatalf("handle FIN: %v", err)
	}

	select {
	case id := <-closed:
		if id != 9 {
			t.Fatalf("closed stream id = %d, want 9", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onStreamClosed was not called for a remote FIN")
	}
	if got := session.ActiveStreamCount(); got != 0 {
		t.Fatalf("active streams = %d, want 0", got)
	}
}

func TestSessionOnStreamClosedHookRunsOutsideStreamLock(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	stream := newStream(3, session)
	session.streams[3] = stream

	done := make(chan struct{})
	session.SetOnStreamClosed(func(id uint32) {
		// These callbacks take streamMu; they would deadlock if the hook ran
		// while removeStream still held the lock.
		if count := session.ActiveStreamCount(); count != 0 {
			t.Errorf("active streams inside callback = %d, want 0", count)
		}
		session.IsIdle()
		close(done)
	})

	go func() {
		if err := stream.Close(); err != nil {
			t.Errorf("close stream: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onStreamClosed callback deadlocked on streamMu")
	}
}

func TestSessionWithoutOnStreamClosedHook(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	stream := newStream(5, session)
	session.streams[5] = stream

	// No hook set: closing must not panic.
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
}

func TestSessionWindowUpdateIncreasesSendWindow(t *testing.T) {
	session := NewSession(&testReadWriteCloser{}, 1)
	session.EnableFlowControl(1024, 1024)
	stream := newStream(11, session)
	stream.initFlowControl(1024, 1024)
	session.streams[11] = stream

	// Exhaust initial send window.
	_, grant := stream.acquireSendWindow(1024)
	if grant != 1024 {
		t.Fatalf("initial grant = %d, want 1024", grant)
	}
	_, grant = stream.acquireSendWindow(100)
	if grant != 0 {
		t.Fatalf("exhausted grant = %d, want 0", grant)
	}

	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], 512)
	if err := session.handleFrame(NewFrame(CmdWindowUpdate, 11, payload[:])); err != nil {
		t.Fatalf("handle WINDOW_UPDATE: %v", err)
	}

	_, grant = stream.acquireSendWindow(600)
	if grant != 512 {
		t.Fatalf("post-update grant = %d, want 512", grant)
	}
}

func TestSessionFastOpenSuccessAndLateRejection(t *testing.T) {
	cConn, sConn := tcpPipeProtocol(t)
	clientSess := NewSession(cConn, 1)
	clientSess.EnableFastOpen()
	defer clientSess.Close()
	defer sConn.Close()

	go func() { _ = clientSess.RunEventLoop() }()

	// Peer intentionally withholds CmdSYNACK for stream 1 to prove OpenStream
	// returns immediately (0-RTT), then rejects stream 2 via late CmdSYNACK error.
	peerFrames := make(chan *Frame, 8)
	go func() {
		fr := NewFrameReader(sConn)
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			peerFrames <- f
			if f.Command == CmdSYN && f.StreamID == 2 {
				_ = EncodeFrame(sConn, NewFrame(CmdSYNACK, 2, []byte("too many streams")))
			}
		}
	}()

	st1, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("fast-open stream 1 failed: %v", err)
	}
	if _, err := st1.Write([]byte("0rtt-payload")); err != nil {
		t.Fatalf("0-RTT write before SYNACK failed: %v", err)
	}
	_ = st1.Close()

	// Drain stream 1 SYN + PSH + FIN; ensure PSH arrived immediately.
	sawPSH := false
	deadline := time.After(2 * time.Second)
	for !sawPSH {
		select {
		case f := <-peerFrames:
			if f.Command == CmdPSH && f.StreamID == 1 && string(f.Data) == "0rtt-payload" {
				sawPSH = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for 0-RTT PSH payload")
		}
	}

	st2, err := clientSess.OpenStream()
	if err != nil {
		t.Fatalf("fast-open stream 2 failed: %v", err)
	}
	defer st2.Close()

	var buf [16]byte
	_, err = st2.Read(buf[:])
	if !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("late rejected fast-open stream read err = %v, want ErrStreamRejected", err)
	}
}

func TestStreamRingRecvQueueWrapAroundAndPoolRecycle(t *testing.T) {
	stream := newStream(1, NewSession(&testReadWriteCloser{}, 1))
	stream.setMaxRecvBytes(4 << 20)

	const totalChunks = 64
	var want []byte
	for i := 0; i < totalChunks; i++ {
		msg := bytes.Repeat([]byte{byte('A' + i%26)}, 3000)
		want = append(want, msg...)
		bufp, pooled := getRecvPayloadBuf(len(msg))
		copy(pooled, msg)
		if err := stream.deliverDataPooled(pooled, bufp); err != nil {
			t.Fatalf("deliver chunk %d: %v", i, err)
		}
	}
	stream.closeByRemote(nil)

	got := make([]byte, 0, len(want))
	tmp := make([]byte, 1000) // partial reads exercise recvLeftChunk recycling
	for {
		n, err := stream.Read(tmp)
		if n > 0 {
			got = append(got, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ring recv mismatch: got %d bytes, want %d", len(got), len(want))
	}
}
