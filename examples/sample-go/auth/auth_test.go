package auth

import "testing"

type fakeStore struct{}

func (fakeStore) FindUser(username string) (User, bool) { return User{Username: username}, true }

func TestLogin(t *testing.T) {
	service := Service{Store: fakeStore{}}
	user, err := service.Login("lloyd", "secret")
	if err != nil || user.Username != "lloyd" {
		t.Fatalf("unexpected login: %#v %v", user, err)
	}
}
