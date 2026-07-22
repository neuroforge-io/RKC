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
	FindUser(username string) (User, bool)
}

type Service struct {
	Store Store
}

// Login validates a user name and returns the matching account.
func (s Service) Login(username, password string) (User, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return User{}, ErrInvalidCredentials
	}
	user, ok := s.Store.FindUser(username)
	if !ok {
		return User{}, ErrInvalidCredentials
	}
	return user, nil
}
