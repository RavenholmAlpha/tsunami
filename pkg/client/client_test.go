package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

func TestApplyPaddingSchemeUpdateUpdatesClientAndWriter(t *testing.T) {
	c := &Client{paddingScheme: padding.DefaultScheme()}
	var out bytes.Buffer
	writer := padding.NewWriter(&out, c.currentPaddingScheme())

	schemeText := "stop=1\n0=50-50"
	expected, err := padding.Parse(schemeText)
	if err != nil {
		t.Fatalf("parse expected scheme: %v", err)
	}

	if err := c.applyPaddingSchemeUpdate([]byte(schemeText), writer); err != nil {
		t.Fatalf("apply padding update: %v", err)
	}
	if got := c.currentPaddingScheme().MD5(); got != expected.MD5() {
		t.Fatalf("client scheme md5 = %s, want %s", got, expected.MD5())
	}

	if err := writer.WriteFramesWithPadding([]*protocol.Frame{
		protocol.NewFrame(protocol.CmdFIN, 1, nil),
	}); err != nil {
		t.Fatalf("write padded frame: %v", err)
	}
	if out.Len() != 50 {
		t.Fatalf("padded write length = %d, want 50", out.Len())
	}
}

func TestApplyPaddingSchemeUpdateRejectsInvalidScheme(t *testing.T) {
	original := padding.DefaultScheme()
	c := &Client{paddingScheme: original}

	if err := c.applyPaddingSchemeUpdate([]byte("stop=bad"), nil); err == nil {
		t.Fatal("expected invalid scheme error")
	}
	if got := c.currentPaddingScheme().MD5(); got != original.MD5() {
		t.Fatalf("client scheme changed after invalid update: got %s, want %s", got, original.MD5())
	}
}

func TestAuthReadErrorClassifiesTimeoutAndEOF(t *testing.T) {
	timeoutAuthErr := newAuthReadError("example.com:443", fmt.Errorf("read frame: %w", timeoutError{}))
	if timeoutAuthErr.Reason != AuthFailureTimeout {
		t.Fatalf("timeout reason = %s, want %s", timeoutAuthErr.Reason, AuthFailureTimeout)
	}
	if !errors.Is(timeoutAuthErr, protocol.ErrAuthFailed) {
		t.Fatal("timeout auth error should match protocol.ErrAuthFailed")
	}
	if !strings.Contains(timeoutAuthErr.Error(), string(AuthFailureTimeout)) {
		t.Fatalf("timeout auth error missing reason: %v", timeoutAuthErr)
	}

	closedAuthErr := newAuthReadError("example.com:443", io.EOF)
	if closedAuthErr.Reason != AuthFailureConnectionClosed {
		t.Fatalf("EOF reason = %s, want %s", closedAuthErr.Reason, AuthFailureConnectionClosed)
	}
}

func TestUnexpectedAuthFrameErrorIncludesFrameMetadata(t *testing.T) {
	frame := protocol.NewFrame(protocol.CmdPSH, 7, []byte("fallback"))
	authErr := newUnexpectedAuthFrameError("example.com:443", frame)

	if authErr.Reason != AuthFailureUnexpectedFrame {
		t.Fatalf("reason = %s, want %s", authErr.Reason, AuthFailureUnexpectedFrame)
	}
	if authErr.Command != protocol.CmdPSH || authErr.StreamID != 7 || authErr.DataLen != len("fallback") {
		t.Fatalf("unexpected frame metadata: command=%d stream=%d data_len=%d", authErr.Command, authErr.StreamID, authErr.DataLen)
	}
	if !errors.Is(authErr, protocol.ErrAuthFailed) {
		t.Fatal("unexpected frame auth error should match protocol.ErrAuthFailed")
	}
}
