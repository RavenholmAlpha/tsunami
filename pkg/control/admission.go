package control

import (
	"context"
	"errors"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// ErrAdmissionDenied means the panel rejected an otherwise authenticated
// session, for example because it would exceed the configured device limit.
var ErrAdmissionDenied = errors.New("control: session admission denied")

// SessionLease represents a control-plane lease held for an authenticated
// connection. The server renews it while the connection is alive and releases
// it on normal session shutdown. Expiry remains the fallback for crashes.
type SessionLease interface {
	Renew(ctx context.Context) error
	Release(ctx context.Context) error
}

// SessionAdmission authorizes an authenticated TSUNAMI session before the
// server acknowledges it to the client.
type SessionAdmission interface {
	Admit(ctx context.Context, user *protocol.UserInfo, clientIP, sessionID string) (SessionLease, error)
}
