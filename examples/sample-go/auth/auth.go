// Package auth demonstrates the deterministic Go extractor.
package auth

import (
	"errors"
	"strings"
)

// ErrInvalidCredentials deliberately gives callers one indistinguishable
// failure for empty, unknown, and incorrect credentials.
var ErrInvalidCredentials = errors.New("invalid credentials")

// User is the authenticated identity returned by the example store; password
// material is intentionally absent.
type User struct {
	Username string
}

// Store is the credential-verification boundary. Implementations own password
// hashing and comparison and reveal only a user plus success bit.
type Store interface {
	// Authenticate must verify the supplied password using the store's
	// password-hashing and constant-time comparison policy.
	Authenticate(username, password string) (User, bool)
}

// Service validates request shape before delegating authentication to Store.
// A nil store fails with ErrInvalidCredentials rather than panicking.
type Service struct {
	Store Store
}

// Login validates both credentials and returns the authenticated account.
func (s Service) Login(username, password string) (User, error) {
	if s.Store == nil || strings.TrimSpace(username) == "" || password == "" {
		return User{}, ErrInvalidCredentials
	}
	user, ok := s.Store.Authenticate(username, password)
	if !ok {
		return User{}, ErrInvalidCredentials
	}
	return user, nil
}
