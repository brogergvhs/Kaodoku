package server

import (
	"net/http/httptest"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/auth"
)

// TestRequiredPerm locks the route->permission gate so a route can't silently
// lose its guard.
func TestRequiredPerm(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"GET", "/library", auth.PermLibraryView},
		{"GET", "/ui/covers/mangaupdates/55", auth.PermLibraryView},
		{"POST", "/ui/library/add", auth.PermLibraryAdd},
		{"POST", "/ui/library/5/remove", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/9/remove", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/9/rename", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/bulk-rename", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/bulk-rename/preview", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/reset-names", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/9/reset-name", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/delete-range", auth.PermLibraryManage},
		{"POST", "/ui/library/5/chapters/9/read", auth.PermReaderUse},
		{"POST", "/ui/collections", auth.PermLibraryView},
		{"POST", "/ui/library/5/collections/add", auth.PermLibraryView},
		{"GET", "/users", auth.PermUsersManage + "|" + auth.PermUsersApprove},
		{"POST", "/ui/users/3/delete", auth.PermUsersManage},
		{"POST", "/ui/users/3/approve", auth.PermUsersApprove},
		{"POST", "/ui/sources/x/delete", auth.PermSourcesManage},
		{"GET", "/ui/sources/kotatsu", auth.PermSourcesManage},
		{"GET", "/ui/sources/kotatsu/list", auth.PermSourcesManage},
		{"GET", "/ui/sources/kotatsu/table", auth.PermSourcesManage},
		{"POST", "/ui/sources/kotatsu/import", auth.PermSourcesManage},
		{"GET", "/api/v1/jobs", auth.PermJobsView},
		{"GET", "/api/v1/jobs/5", auth.PermJobsView},
		{"POST", "/api/v1/jobs/5/cancel", auth.PermJobsManage},
		{"POST", "/api/v1/jobs/run", auth.PermJobsManage},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := requiredPerm(r); got != c.want {
			t.Errorf("%s %s: perm=%q want %q", c.method, c.path, got, c.want)
		}
	}
}
