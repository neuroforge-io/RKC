// Package auth demonstrates the deterministic Go extractor.
package auth

import (
	"errors"
	"strings"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

type User struct {
	Username string
}

type Store interface {
	// Authenticate must verify the supplied password using the store's
	// password-hashing and constant-time comparison policy.
	Authenticate(username, password string) (User, bool)
}

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
