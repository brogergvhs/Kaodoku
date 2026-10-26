package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/service"
)

// permMeta gives each permission a human name and a description shown when
// composing roles.
func permMeta(perm string) (label, desc string) {
	switch perm {
	case auth.PermLibraryView:
		return "View library", "Browse the library, manga pages and chapter lists."
	case auth.PermStatsView:
		return "View statistics", "See the statistics cards on the dashboard."
	case auth.PermServicesView:
		return "View services health", "See whether FlareSolverr and the browser downloader are reachable."
	case auth.PermJobsView:
		return "View jobs", "See the background jobs list on the dashboard."
	case auth.PermJobsManage:
		return "Manage jobs", "Cancel queued or running background jobs."
	case auth.PermReaderUse:
		return "Read manga", "Open the reader and keep their own reading progress."
	case auth.PermLibraryAdd:
		return "Add manga", "Search metadata backends and add new titles to the library."
	case auth.PermLibraryManage:
		return "Manage library", "Refresh chapters, download, link sources, monitor and remove titles."
	case auth.PermImportUse:
		return "Import from disk", "See untracked download folders and import them."
	case auth.PermSourcesManage:
		return "Manage sources", "Add, edit, verify, disable or delete scraper sources."
	case auth.PermSettingsAppearance:
		return "Personal appearance", "Pick their own theme and custom colors."
	case auth.PermSettingsManage:
		return "Manage settings", "Change global scheduling, job and service settings."
	case auth.PermUsersManage:
		return "Manage users", "Create users and roles and assign permissions."
	case auth.PermUsersApprove:
		return "Approve users", "Approve accounts that signed in via SSO and are awaiting approval."
	case auth.PermMetricsUsers:
		return "View others' metrics", "Open the Metrics page's Users tab and see any user's reading metrics."
	case auth.PermSessionsView:
		return "View sessions", "See who is signed in, from which device, and what they are reading."
	default:
		return perm, ""
	}
}

// permGroup clusters related permissions for the role editors.
type permGroup struct {
	Title string
	Perms []string
}

func permGroups() []permGroup {
	return []permGroup{
		{Title: "Dashboard", Perms: []string{auth.PermStatsView, auth.PermServicesView, auth.PermJobsView, auth.PermSessionsView}},
		{Title: "Library", Perms: []string{auth.PermLibraryView, auth.PermLibraryAdd, auth.PermLibraryManage, auth.PermImportUse}},
		{Title: "Reading", Perms: []string{auth.PermReaderUse}},
		{Title: "Personal", Perms: []string{auth.PermSettingsAppearance}},
		{Title: "Administration", Perms: []string{auth.PermJobsManage, auth.PermSourcesManage, auth.PermSettingsManage, auth.PermUsersManage, auth.PermUsersApprove, auth.PermMetricsUsers}},
	}
}

type usersView struct {
	Users           []auth.User
	Roles           []auth.Role
	Groups          []permGroup
	TagOptions      []catalog.ContentTag
	ProviderOptions []providerOption
}

type providerOption struct{ Name, Label string }

func (u *webUI) providerOptions(names []string) []providerOption {
	var out []providerOption
	for _, name := range names {
		out = append(out, providerOption{Name: name, Label: u.svc.ProviderLabel(name)})
	}
	return out
}

func (u *webUI) usersData(r *http.Request) usersView {
	users, _ := u.svc.Auth().ListUsers(r.Context())
	roles, _ := u.svc.Auth().ListRoles(r.Context())
	opts, _ := u.svc.ContentTagOptions(r.Context())
	return usersView{Users: users, Roles: roles, Groups: permGroups(), TagOptions: opts, ProviderOptions: u.providerOptions(service.ProviderNames())}
}

func (u *webUI) usersPage(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, "users", "Users", u.usersData(r))
}

func (u *webUI) usersFrag(w http.ResponseWriter, r *http.Request) {
	u.frag(w, "usersContent", u.usersData(r))
}

type userEditView struct {
	User            auth.User
	Roles           []auth.Role
	TagOptions      []catalog.ContentTag
	ProviderOptions []providerOption
}

func (u *webUI) userEditModal(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	usr, err := u.svc.Auth().GetUser(r.Context(), id)
	if err != nil || usr == nil {
		u.fail(w, fmt.Errorf("user not found"))
		return
	}
	roles, _ := u.svc.Auth().ListRoles(r.Context())
	opts, _ := u.svc.ContentTagOptions(r.Context())
	u.frag(w, "userEditModal", userEditView{User: *usr, Roles: roles, TagOptions: opts, ProviderOptions: u.providerOptions(service.ProviderNames())})
}

type roleEditView struct {
	Role   auth.Role
	Groups []permGroup
}

func (u *webUI) roleEditModal(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	roles, _ := u.svc.Auth().ListRoles(r.Context())
	for _, role := range roles {
		if role.ID == id {
			u.frag(w, "roleEditModal", roleEditView{Role: role, Groups: permGroups()})
			return
		}
	}
	u.fail(w, fmt.Errorf("role not found"))
}

func contentGuardsFromForm(r *http.Request) auth.ContentGuards {
	_ = r.ParseForm()
	return auth.ContentGuards{
		AllowAdult:       r.FormValue("allow_adult") == "on",
		BlockedTags:      tagList(r.Form["blocked_tags"]),
		AllowedTags:      tagList(r.Form["allowed_tags"]),
		BlockedProviders: r.Form["blocked_providers"],
	}
}

func (u *webUI) userCreate(w http.ResponseWriter, r *http.Request) {
	roleID, _ := strconv.ParseInt(r.FormValue("role_id"), 10, 64)
	if err := u.svc.Auth().CreateUser(r.Context(), r.FormValue("username"), r.FormValue("password"), roleID, contentGuardsFromForm(r)); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (u *webUI) userUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	roleID, _ := strconv.ParseInt(r.FormValue("role_id"), 10, 64)
	if err := u.svc.Auth().UpdateUser(r.Context(), id, roleID, r.FormValue("password"), contentGuardsFromForm(r)); err != nil {
		u.fail(w, err)
		return
	}
	if _, present := r.Form["oidc_subject"]; present && id != auth.EnvAdminID {
		if err := u.svc.Auth().SetOIDCSubject(r.Context(), id, r.FormValue("oidc_subject")); err != nil {
			u.fail(w, err)
			return
		}
	}
	flushBasicCreds()
	w.Header().Set("HX-Refresh", "true")
}

func (u *webUI) userApprove(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.Auth().SetUserPending(r.Context(), id, false); err != nil {
		u.fail(w, err)
		return
	}
	u.usersFrag(w, r)
}

func (u *webUI) userDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.Auth().DeleteUser(r.Context(), id); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (u *webUI) roleSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err := u.svc.Auth().SaveRole(r.Context(), id, r.FormValue("name"), r.Form["perm"]); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (u *webUI) roleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.Auth().DeleteRole(r.Context(), id); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}
