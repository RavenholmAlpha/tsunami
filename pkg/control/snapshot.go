// Package control defines the control-plane extension points used to connect
// TSUNAMI nodes with panels, board systems, and future subscription adapters.
package control

import (
	"context"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// Snapshot is the normalized user/config view produced by an adapter.
type Snapshot struct {
	Source  string
	Version string
	Full    bool

	FetchedAt time.Time

	Users          []*protocol.UserInfo
	DeletedUserIDs []string

	Metadata map[string]string
}

// Clone returns a deep enough copy for middleware and store application.
func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return nil
	}
	cloned := *s
	if s.Users != nil {
		cloned.Users = make([]*protocol.UserInfo, 0, len(s.Users))
		for _, u := range s.Users {
			cloned.Users = append(cloned.Users, u.Clone())
		}
	}
	if s.DeletedUserIDs != nil {
		cloned.DeletedUserIDs = append([]string(nil), s.DeletedUserIDs...)
	}
	if s.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(s.Metadata))
		for k, v := range s.Metadata {
			cloned.Metadata[k] = v
		}
	}
	return &cloned
}

// Adapter fetches panel or board data and converts it to a normalized Snapshot.
type Adapter interface {
	Name() string
	FetchSnapshot(ctx context.Context) (*Snapshot, error)
}

// StaticAdapter exposes an in-memory user list through the same adapter shape
// used by future Xboard, V2board, custom panel, and subscription adapters.
type StaticAdapter struct {
	name     string
	snapshot *Snapshot
}

// NewStaticAdapter creates a static adapter from users.
func NewStaticAdapter(name string, users []*protocol.UserInfo) *StaticAdapter {
	if name == "" {
		name = "static"
	}
	return &StaticAdapter{
		name: name,
		snapshot: &Snapshot{
			Source:    name,
			Full:      true,
			FetchedAt: time.Now(),
			Users:     users,
		},
	}
}

// Name returns the adapter name.
func (a *StaticAdapter) Name() string {
	if a == nil || a.name == "" {
		return "static"
	}
	return a.name
}

// FetchSnapshot returns the static user snapshot.
func (a *StaticAdapter) FetchSnapshot(ctx context.Context) (*Snapshot, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	snapshot := a.snapshot.Clone()
	if snapshot.Source == "" {
		snapshot.Source = a.Name()
	}
	snapshot.FetchedAt = time.Now()
	return snapshot, nil
}
