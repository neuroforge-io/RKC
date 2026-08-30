package auth

import "testing"

type fakeStore struct{ found bool }

func (store fakeStore) FindUser(username string) (User, bool) {
	return User{Username: username}, store.found
}

func TestLogin(t *testing.T) {
	service := Service{Store: fakeStore{found: true}}
	user, err := service.Login("lloyd", "secret")
	if err != nil || user.Username != "lloyd" {
		t.Fatalf("unexpected login: %#v %v", user, err)
	}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	for name, testCase := range map[string]struct {
		service            Service
		username, password string
	}{
		"blank username": {Service{Store: fakeStore{found: true}}, "  ", "secret"},
		"blank password": {Service{Store: fakeStore{found: true}}, "lloyd", ""},
		"unknown user":   {Service{Store: fakeStore{found: false}}, "lloyd", "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := testCase.service.Login(testCase.username, testCase.password); err != ErrInvalidCredentials {
				t.Fatalf("Login(%q, %q) error = %v", testCase.username, testCase.password, err)
			}
		})
	}
}
