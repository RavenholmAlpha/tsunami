package client

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// AuthFailureReason classifies why the client did not receive the expected
// server-settings frame after sending authentication.
type AuthFailureReason string

const (
	AuthFailureTimeout          AuthFailureReason = "timeout_waiting_for_server_settings"
	AuthFailureConnectionClosed AuthFailureReason = "connection_closed_during_auth_confirmation"
	AuthFailureUnexpectedFrame  AuthFailureReason = "unexpected_auth_confirmation_frame"
	AuthFailureReadError        AuthFailureReason = "invalid_auth_confirmation_frame"
)

// AuthError preserves a machine-readable authentication failure reason while
// still matching protocol.ErrAuthFailed via errors.Is.
type AuthError struct {
	ServerAddr string
	Reason     AuthFailureReason
	Err        error

	Command  protocol.Command
	StreamID uint32
	DataLen  int
}

func (e *AuthError) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := fmt.Sprintf("tsunami: authentication failed server=%s reason=%s hint=%q", e.ServerAddr, e.Reason, e.Hint())
	if e.Reason == AuthFailureUnexpectedFrame {
		return fmt.Sprintf("%s command=%d stream_id=%d data_len=%d", base, e.Command, e.StreamID, e.DataLen)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s cause=%v", base, e.Err)
	}
	return base
}

func (e *AuthError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *AuthError) Is(target error) bool {
	return target == protocol.ErrAuthFailed
}

func (e *AuthError) Hint() string {
	if e == nil {
		return ""
	}
	switch e.Reason {
	case AuthFailureTimeout:
		return "wrong password, fallback response, or non-TSUNAMI service"
	case AuthFailureConnectionClosed:
		return "wrong password or server closed the auth connection"
	case AuthFailureUnexpectedFrame:
		return "server returned non-auth settings, likely fallback bytes or protocol mismatch"
	default:
		return "invalid or truncated auth confirmation from server"
	}
}

func newAuthReadError(serverAddr string, err error) *AuthError {
	reason := AuthFailureReadError
	var netErr net.Error
	switch {
	case errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed):
		reason = AuthFailureConnectionClosed
	case errors.As(err, &netErr) && netErr.Timeout():
		reason = AuthFailureTimeout
	}
	return &AuthError{
		ServerAddr: serverAddr,
		Reason:     reason,
		Err:        err,
	}
}

func newUnexpectedAuthFrameError(serverAddr string, frame *protocol.Frame) *AuthError {
	authErr := &AuthError{
		ServerAddr: serverAddr,
		Reason:     AuthFailureUnexpectedFrame,
	}
	if frame != nil {
		authErr.Command = frame.Command
		authErr.StreamID = frame.StreamID
		authErr.DataLen = len(frame.Data)
	}
	return authErr
}
