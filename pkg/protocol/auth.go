package protocol

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
)

// PasswordHash computes SHA-256(password) used for authentication.
func PasswordHash(password string) [AuthHashLen]byte {
	return sha256.Sum256([]byte(password))
}

// AuthRequest represents the client authentication payload.
//
// Wire format:
//
//	+-------------------+-------------------+-----------+
//	| SHA-256(password) | padding0 length   | padding0  |
//	+-------------------+-------------------+-----------+
//	| 32 Bytes          | uint16 Big-Endian | variable  |
//	+-------------------+-------------------+-----------+
type AuthRequest struct {
	Hash    [AuthHashLen]byte
	Padding []byte
}

// EncodeAuthRequest writes the authentication request in a single write call
// to avoid predictable packet size patterns.
func EncodeAuthRequest(w io.Writer, hash [AuthHashLen]byte, padding []byte) error {
	paddingLen := len(padding)
	buf := make([]byte, AuthHashLen+2+paddingLen)

	copy(buf[:AuthHashLen], hash[:])
	binary.BigEndian.PutUint16(buf[AuthHashLen:AuthHashLen+2], uint16(paddingLen))
	if paddingLen > 0 {
		copy(buf[AuthHashLen+2:], padding)
	}

	_, err := w.Write(buf)
	return err
}

// DecodeAuthRequest reads and parses the authentication request.
func DecodeAuthRequest(r io.Reader) (*AuthRequest, error) {
	// Read hash + padding length (32 + 2 = 34 bytes)
	var header [AuthHashLen + 2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("tsunami: read auth header: %w", err)
	}

	req := &AuthRequest{}
	copy(req.Hash[:], header[:AuthHashLen])
	paddingLen := binary.BigEndian.Uint16(header[AuthHashLen : AuthHashLen+2])

	if paddingLen > 0 {
		req.Padding = make([]byte, paddingLen)
		if _, err := io.ReadFull(r, req.Padding); err != nil {
			return nil, fmt.Errorf("tsunami: read auth padding: %w", err)
		}
	}

	return req, nil
}

// AuthOverhead is the fixed overhead of the authentication message: hash(32) + paddingLen(2) = 34.
const AuthOverhead = AuthHashLen + 2

// Authenticator handles server-side user authentication.
type Authenticator struct {
	users map[[AuthHashLen]byte]*UserInfo
}

// UserInfo holds per-user metadata.
type UserInfo struct {
	Name      string
	Password  string
	Bandwidth int // Mbps, 0 = unlimited
}

// NewAuthenticator creates a new Authenticator with the given user list.
func NewAuthenticator(users []*UserInfo) *Authenticator {
	a := &Authenticator{
		users: make(map[[AuthHashLen]byte]*UserInfo, len(users)),
	}
	for _, u := range users {
		hash := PasswordHash(u.Password)
		a.users[hash] = u
	}
	return a
}

// Authenticate checks the provided hash against known users.
// Returns the matched UserInfo or nil if authentication fails.
func (a *Authenticator) Authenticate(hash [AuthHashLen]byte) *UserInfo {
	for knownHash, user := range a.users {
		if subtle.ConstantTimeCompare(hash[:], knownHash[:]) == 1 {
			return user
		}
	}
	return nil
}
