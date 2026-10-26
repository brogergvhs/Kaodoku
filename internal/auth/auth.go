// Package auth provides users, roles, permissions and cookie sessions.
package auth

import (
	"context"
	"slices"
	"time"
)

// Permission identifiers. Roles carry a set of these.
const (
	PermLibraryView        = "library.view"        // library, title pages, chapter lists
	PermStatsView          = "stats.view"          // dashboard statistics
	PermServicesView       = "services.view"       // dashboard services health
	PermSessionsView       = "sessions.view"       // who is signed in / presence
	PermJobsView           = "jobs.view"           // dashboard jobs list
	PermReaderUse          = "reader.use"          // reader + own read marking
	PermLibraryAdd         = "library.add"         // search page + adding titles
	PermLibraryManage      = "library.manage"      // title mutations (remove, refresh, download, linking...)
	PermImportUse          = "import.use"          // import page + importing folders
	PermSourcesManage      = "sources.manage"      // sources administration
	PermJobsManage         = "jobs.manage"         // cancelling jobs
	PermSettingsAppearance = "settings.appearance" // own theme settings
	PermSettingsManage     = "settings.manage"     // global settings sections
	PermUsersManage        = "users.manage"        // users & roles administration
	PermUsersApprove       = "users.approve"       // approve pending (auto-provisioned) accounts
	PermMetricsUsers       = "metrics.users"       // view other users' reading metrics
)

// Permissions lists every known permission (stable order, for UIs).
func Permissions() []string {
	return []string{
		PermLibraryView, PermStatsView, PermServicesView, PermSessionsView, PermJobsView,
		PermReaderUse, PermLibraryAdd, PermLibraryManage, PermImportUse,
		PermSourcesManage, PermJobsManage,
		PermSettingsAppearance, PermSettingsManage, PermUsersManage, PermUsersApprove, PermMetricsUsers,
	}
}

// EnvAdminID is the fixed user id of the env-provisioned admin.
const EnvAdminID int64 = 1

const (
	OriginEnv     = "env"
	OriginLocal   = "local"
	OriginBuiltin = "builtin"
	OriginOIDC    = "oidc"
)

type User struct {
	ID               int64
	Username         string
	Origin           string // env|local
	RoleID           int64
	RoleName         string
	OIDCSubject      string
	Pending          bool // provisioned but awaiting admin approval
	AllowAdult       bool
	BlockedTags      []string // tags/genres this user must not see
	AllowedTags      []string // when set, only manga carrying one of these are shown
	BlockedProviders []string
	Perms            map[string]bool
	CreatedAt        time.Time
}

// Can reports whether the user holds a permission.
func (u *User) Can(perm string) bool {
	if u == nil {
		return false
	}
	return u.Perms[perm]
}

func (u *User) ProviderAllowed(name string) bool {
	return u == nil || !slices.Contains(u.BlockedProviders, name)
}

// IsEnvAdmin reports whether this is the env-provisioned admin (undeletable,
// env-owned credentials). Value receiver: templates call it on []User values.
func (u User) IsEnvAdmin() bool { return u.ID == EnvAdminID && u.Origin == OriginEnv }

type Role struct {
	ID     int64
	Name   string
	Origin string // builtin|local
	Perms  []string
}

// builtinRoles seeds the fixed role set; the admin role always holds every
// permission (kept in sync at boot even if the permission list grows).
func builtinRoles() []Role {
	return []Role{
		{Name: "admin", Origin: OriginBuiltin, Perms: Permissions()},
		{Name: "member", Origin: OriginBuiltin, Perms: []string{
			PermLibraryView, PermReaderUse,
			PermLibraryAdd, PermLibraryManage, PermSettingsAppearance,
		}},
		{Name: "viewer", Origin: OriginBuiltin, Perms: []string{
			PermLibraryView, PermReaderUse, PermSettingsAppearance,
		}},
	}
}

// --- request context plumbing ---

type ctxUserKey struct{}

// WithUser stores the authenticated user in a context.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxUserKey{}, u)
}

// FromContext returns the authenticated user, or nil.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxUserKey{}).(*User)
	return u
}

// UserID returns the acting user's id; background jobs and CLI contexts that
// carry no user act as the env admin.
func UserID(ctx context.Context) int64 {
	if u := FromContext(ctx); u != nil {
		return u.ID
	}
	return EnvAdminID
}
