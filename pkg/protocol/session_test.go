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
