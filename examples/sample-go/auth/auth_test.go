package auth

import (
	"crypto/subtle"
	"testing"
)

type fakeStore struct {
	username string
	password string
}

func (store fakeStore) Authenticate(username, password string) (User, bool) {
	usernameMatch := subtle.ConstantTimeCompare([]byte(username), []byte(store.username))
	passwordMatch := subtle.ConstantTimeCompare([]byte(password), []byte(store.password))
	if usernameMatch != 1 || passwordMatch != 1 {
		return User{}, false
	}
	return User{Username: username}, true
}

func TestLogin(t *testing.T) {
	service := Service{Store: fakeStore{username: "sample-user", password: "correct horse"}}
	user, err := service.Login("sample-user", "correct horse")
	if err != nil || user.Username != "sample-user" {
		t.Fatalf("unexpected login: %#v %v", user, err)
	}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	for name, testCase := range map[string]struct {
		service            Service
		username, password string
	}{
		"blank username": {Service{Store: fakeStore{username: "sample-user", password: "correct horse"}}, "  ", "correct horse"},
		"blank password": {Service{Store: fakeStore{username: "sample-user", password: "correct horse"}}, "sample-user", ""},
		"missing store":  {Service{}, "sample-user", "correct horse"},
		"unknown user":   {Service{Store: fakeStore{username: "sample-user", password: "correct horse"}}, "unknown", "correct horse"},
		"wrong password": {Service{Store: fakeStore{username: "sample-user", password: "correct horse"}}, "sample-user", "wrong"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := testCase.service.Login(testCase.username, testCase.password); err != ErrInvalidCredentials {
				t.Fatalf("Login(%q, %q) error = %v", testCase.username, testCase.password, err)
			}
		})
	}
}
