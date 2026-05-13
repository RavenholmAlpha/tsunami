package protocol

import (
	"bytes"
	"errors"
	"testing"
)

type testReadWriteCloser struct {
	bytes.Buffer
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
