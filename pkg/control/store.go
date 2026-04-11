package control

import (
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

type authEntry struct {
	hash [protocol.AuthHashLen]byte
	user *protocol.UserInfo
}

type userStoreState struct {
	version   string
	updatedAt time.Time
	entries   []authEntry
	byID      map[string]*protocol.UserInfo
}

// AuthResult describes an authentication decision.
type AuthResult struct {
	User   *protocol.UserInfo
	Reason string
}

// UserStore is an atomically replaceable runtime user table.
type UserStore struct {
	state atomic.Value
}

// NewUserStore creates a store from a static user list.
func NewUserStore(users []*protocol.UserInfo) (*UserStore, error) {
	store := NewEmptyUserStore()
	if err := store.ApplySnapshot(&Snapshot{Full: true, Users: users}); err != nil {
		return nil, err
	}
	return store, nil
}

// NewEmptyUserStore creates an empty store.
func NewEmptyUserStore() *UserStore {
	store := &UserStore{}
	store.state.Store(userStoreState{
		updatedAt: time.Now(),
		byID:      make(map[string]*protocol.UserInfo),
	})
	return store
}

// ApplySnapshot atomically applies a full or incremental user snapshot.
func (s *UserStore) ApplySnapshot(snapshot *Snapshot) error {
	if s == nil {
		return fmt.Errorf("control: nil user store")
	}
	if snapshot == nil {
		return fmt.Errorf("control: nil snapshot")
	}

	current := s.current()
	nextByID := make(map[string]*protocol.UserInfo)

	if !snapshot.Full {
		for id, user := range current.byID {
			nextByID[id] = user.Clone()
		}
	}

	for _, id := range snapshot.DeletedUserIDs {
		delete(nextByID, id)
	}

	for _, user := range snapshot.Users {
		if user == nil {
			return fmt.Errorf("control: nil user in snapshot")
		}
		hash, err := user.AuthHash()
		if err != nil {
			return err
		}
		nextByID[userStoreKey(user, hash)] = user.Clone()
	}

	entries := make([]authEntry, 0, len(nextByID))
	seenHashes := make(map[[protocol.AuthHashLen]byte]string, len(nextByID))
	for key, user := range nextByID {
		hash, err := user.AuthHash()
		if err != nil {
			return err
		}
		if prev, ok := seenHashes[hash]; ok {
			return fmt.Errorf("control: duplicate auth hash for users %q and %q", prev, key)
		}
		seenHashes[hash] = key
		entries = append(entries, authEntry{
			hash: hash,
			user: user.Clone(),
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].user.Identity() < entries[j].user.Identity()
	})

	version := snapshot.Version
	if version == "" {
		version = current.version
	}

	s.state.Store(userStoreState{
		version:   version,
		updatedAt: time.Now(),
		entries:   entries,
		byID:      nextByID,
	})
	return nil
}

// Authenticate returns the matched user when auth and validity checks pass.
func (s *UserStore) Authenticate(hash [protocol.AuthHashLen]byte) *protocol.UserInfo {
	result := s.AuthenticateDetailed(hash)
	return result.User
}

// AuthenticateDetailed returns the matched user or a rejection reason.
func (s *UserStore) AuthenticateDetailed(hash [protocol.AuthHashLen]byte) AuthResult {
	if s == nil {
		return AuthResult{Reason: "nil store"}
	}

	state := s.current()
	now := time.Now()
	for _, entry := range state.entries {
		if subtle.ConstantTimeCompare(hash[:], entry.hash[:]) != 1 {
			continue
		}
		if ok, reason := entry.user.IsUsable(now); !ok {
			return AuthResult{Reason: reason}
		}
		return AuthResult{User: entry.user.Clone()}
	}
	return AuthResult{Reason: "not found"}
}

// Snapshot returns the current in-memory snapshot.
func (s *UserStore) Snapshot() *Snapshot {
	state := s.current()
	users := make([]*protocol.UserInfo, 0, len(state.entries))
	for _, entry := range state.entries {
		users = append(users, entry.user.Clone())
	}
	return &Snapshot{
		Version:   state.version,
		Full:      true,
		FetchedAt: state.updatedAt,
		Users:     users,
	}
}

// Version returns the last applied config version.
func (s *UserStore) Version() string {
	return s.current().version
}

func (s *UserStore) current() userStoreState {
	if s == nil {
		return userStoreState{byID: make(map[string]*protocol.UserInfo)}
	}
	value := s.state.Load()
	if value == nil {
		return userStoreState{byID: make(map[string]*protocol.UserInfo)}
	}
	return value.(userStoreState)
}

func userStoreKey(user *protocol.UserInfo, hash [protocol.AuthHashLen]byte) string {
	if user.ID != "" {
		return user.ID
	}
	if user.Name != "" {
		return user.Name
	}
	return hex.EncodeToString(hash[:])
}
