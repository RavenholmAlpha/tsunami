package server

import (
	"context"
	"crypto/sha256"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type admissionRecorder struct {
	allowed bool
	calls   chan admissionCall
}

type admissionCall struct {
	userID    string
	clientIP  string
	sessionID string
}

func (r *admissionRecorder) Admit(_ context.Context, user *protocol.UserInfo, clientIP, sessionID string) (control.SessionLease, error) {
	r.calls <- admissionCall{userID: user.ID, clientIP: clientIP, sessionID: sessionID}
	if !r.allowed {
		return nil, control.ErrAdmissionDenied
	}
	return testLease{}, nil
}

type testLease struct{}

func (testLease) Renew(context.Context) error   { return nil }
func (testLease) Release(context.Context) error { return nil }

type remoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteAddrConn) RemoteAddr() net.Addr { return c.remote }

func TestSessionAdmissionPrecedesServerSettings(t *testing.T) {
	user := &protocol.UserInfo{ID: "1001", Name: "alice", Password: "user-secret"}
	recorder := &admissionRecorder{allowed: true, calls: make(chan admissionCall, 1)}
	srv, err := New(Config{
		Users:            []*protocol.UserInfo{user},
		SessionAdmission: recorder,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	wrapped := remoteAddrConn{
		Conn:   serverConn,
		remote: &net.TCPAddr{IP: net.ParseIP("::ffff:203.0.113.7"), Port: 54321},
	}
	done := make(chan struct{})
	go func() {
		srv.handleConn(wrapped)
		close(done)
	}()

	writeAuthenticatedSettings(t, clientConn, "user-secret")
	frame, err := protocol.DecodeFrame(clientConn)
	if err != nil {
		t.Fatalf("read server settings: %v", err)
	}
	if frame.Command != protocol.CmdServerSettings {
		t.Fatalf("first server frame = %s, want SERVER_SETTINGS", frame.Command)
	}

	select {
	case call := <-recorder.calls:
		if call.userID != "1001" || call.clientIP != "203.0.113.7" || len(call.sessionID) != 32 {
			t.Fatalf("admission call = %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("admission was not called")
	}

	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server connection handler did not exit")
	}
}

func TestRejectedSessionDoesNotReceiveServerSettings(t *testing.T) {
	user := &protocol.UserInfo{ID: "1001", Name: "alice", Password: "user-secret"}
	recorder := &admissionRecorder{allowed: false, calls: make(chan admissionCall, 1)}
	srv, err := New(Config{
		Users:            []*protocol.UserInfo{user},
		SessionAdmission: recorder,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	wrapped := remoteAddrConn{
		Conn:   serverConn,
		remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.8"), Port: 54321},
	}
	done := make(chan struct{})
	go func() {
		srv.handleConn(wrapped)
		close(done)
	}()

	writeAuthenticatedSettings(t, clientConn, "user-secret")
	_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = protocol.DecodeFrame(clientConn)
	if err == nil {
		t.Fatal("rejected session received a server frame")
	}
	if err != io.EOF {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			t.Fatalf("rejected session did not close: %v", err)
		}
	}

	select {
	case <-recorder.calls:
	case <-time.After(time.Second):
		t.Fatal("admission was not called")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server connection handler did not exit")
	}
}

func TestNormalizedRemoteIP(t *testing.T) {
	tests := map[string]struct {
		input string
		want  string
	}{
		"ipv4":        {input: "203.0.113.9:443", want: "203.0.113.9"},
		"ipv6":        {input: "[2001:db8::9]:443", want: "2001:db8::9"},
		"mapped ipv6": {input: "[::ffff:203.0.113.9]:443", want: "203.0.113.9"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := normalizedRemoteIP(testAddr(test.input))
			if err != nil || got != test.want {
				t.Fatalf("normalizedRemoteIP(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
		})
	}
}

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func writeAuthenticatedSettings(t *testing.T, conn net.Conn, password string) {
	t.Helper()
	hash := sha256.Sum256([]byte(password))
	if err := protocol.EncodeAuthRequest(conn, hash, nil); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	settings := protocol.EncodeClientSettings(&protocol.ClientSettings{
		Version: protocol.CurrentVersion,
		Client:  "test-client",
	})
	if err := protocol.EncodeFrame(conn, protocol.NewFrame(protocol.CmdSettings, 0, settings)); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}
