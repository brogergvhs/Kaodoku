package service

import (
	"testing"

	"github.com/brogergvhs/kaodoku/internal/auth"
)

func TestContentAllowedFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		user    *auth.User
		isAdult bool
		tags    []string
		want    bool
	}{
		{"nil user", nil, false, nil, false},
		{"adult blocked", &auth.User{}, true, nil, false},
		{"adult allowed", &auth.User{AllowAdult: true}, true, nil, true},
		{"plain title", &auth.User{}, false, []string{"Action"}, true},
		{"blocked tag", &auth.User{BlockedTags: []string{"Ecchi"}}, false, []string{"Action", "Ecchi"}, false},
		{"allowed-list miss", &auth.User{AllowedTags: []string{"Romance"}}, false, []string{"Action"}, false},
		{"allowed-list hit", &auth.User{AllowedTags: []string{"Romance"}}, false, []string{"Romance"}, true},
	}
	for _, c := range cases {
		if got := ContentAllowed(c.user, c.isAdult, c.tags); got != c.want {
			t.Errorf("%s: contentAllowedFor = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOwnsTitle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		userID, addedBy int64
		want            bool
	}{
		{"own title", 2, 2, true},
		{"someone else's", 3, 2, false},
		{"legacy owned by admin", auth.EnvAdminID, 0, true},
		{"legacy not for other user", 2, 0, false},
	}
	for _, c := range cases {
		if got := ownsTitle(c.userID, c.addedBy); got != c.want {
			t.Errorf("%s: ownsTitle(%d,%d) = %v, want %v", c.name, c.userID, c.addedBy, got, c.want)
		}
	}
}
