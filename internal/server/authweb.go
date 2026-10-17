package server

import (
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/service"
)

// userFrom returns the authenticated user for a request (never nil once the
// auth middleware has run).
func userFrom(ctx context.Context) *auth.User { return auth.FromContext(ctx) }

// authEnabled reports whether username/password auth is active.
func authEnabled() bool { return os.Getenv("KAODOKU_ADMIN_PASSWORD") != "" }

// requireUser resolves the request's user and stores it in the context:
// session cookie when auth is enabled, the env admin otherwise.
func requireUser(next http.Handler, svc *service.JobService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/login" ||
			strings.HasPrefix(r.URL.Path, "/auth/oidc/") ||
			r.URL.Path == "/api/v1/meta" || r.URL.Path == "/api/v1/auth/login" ||
			r.URL.Path == "/komga/api/v1/claim" ||
			r.URL.Path == "/komga/api/v1/client-settings/global/list" ||
			r.URL.Path == "/komga/api/v1/oauth2/providers" {
			next.ServeHTTP(w, r)
			return
		}
		user := resolveUser(r, svc)
		if user == nil {
			if strings.HasPrefix(r.URL.Path, "/opds") || strings.HasPrefix(r.URL.Path, "/komga") {
				w.Header().Set("WWW-Authenticate", `Basic realm="Kaodoku"`)
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/v1/") {
				v1err(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			} else {
				writeError(w, http.StatusUnauthorized, "unauthorized")
			}
			return
		}
		if user.Pending {
			if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(renderLoginPage(`<p class="text-sm">Your account is waiting for an admin's approval.</p>`)))
				return
			}
			writeError(w, http.StatusForbidden, "account pending approval")
			return
		}
		if p := requiredPerm(r); p != "" && !canAny(user, p) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/") {
				v1err(w, http.StatusForbidden, "forbidden", "missing permission: "+p)
			} else {
				writeError(w, http.StatusForbidden, "missing permission: "+p)
			}
			return
		}
		if !user.AllowAdult {
			if adult := svc.AdultTagNames(r.Context()); len(adult) > 0 {
				user.BlockedTags = append(append([]string{}, user.BlockedTags...), adult...)
			}
		}
		ctx := auth.WithUser(r.Context(), user)
		if c, err := r.Cookie("kaodoku_session"); err == nil {
			ctx = withSessionKey(ctx, c.Value)
		} else if t := headerToken(r); t != "" {
			ctx = withSessionKey(ctx, t)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requiredPerm maps a request to the permission it needs. Central and
// deny-by-default: unknown mutations require library.manage.
func requiredPerm(r *http.Request) string {
	p := r.URL.Path
	switch {
	case p == "/management" || p == "/metrics" || p == "/ui/metrics":
		return ""
	case p == "/api/v1/meta" || p == "/api/v1/auth/login" || p == "/api/v1/me" || p == "/api/v1/auth/token" || p == "/api/v1/me/settings":
		return "" // public or any-signed-in
	case p == "/api/v1/jobs/run":
		return auth.PermJobsManage
	case p == "/api/v1/jobs/enqueue":
		return "" // ownership scoping in the handler
	case strings.HasPrefix(p, "/api/v1/jobs/") && strings.HasSuffix(p, "/cancel"):
		return auth.PermJobsManage
	case strings.HasPrefix(p, "/api/v1/jobs"):
		return auth.PermJobsView
	case p == "/api/v1/wanted/track":
		return auth.PermLibraryManage
	case p == "/api/v1/tags": // filter vocabulary, needed by library viewers too
		return auth.PermLibraryView
	case strings.HasPrefix(p, "/api/v1/sources/"):
		return auth.PermSourcesManage
	case strings.HasPrefix(p, "/api/v1/library/") && strings.Contains(p, "/sources"):
		return auth.PermLibraryManage
	case strings.HasPrefix(p, "/api/v1/wanted") || p == "/api/v1/library/add":
		return auth.PermLibraryAdd
	case strings.HasPrefix(p, "/api/v1/anilist"):
		return auth.PermReaderUse
	case strings.HasPrefix(p, "/api/v1/reader/"):
		return auth.PermReaderUse
	case strings.HasPrefix(p, "/opds") || strings.HasPrefix(p, "/komga"):
		return auth.PermReaderUse
	case strings.HasPrefix(p, "/api/v1/library") && strings.HasSuffix(p, "/favourite"):
		return auth.PermLibraryView
	case strings.HasPrefix(p, "/api/v1/library") && r.Method != http.MethodGet:
		return auth.PermLibraryManage
	case strings.HasPrefix(p, "/api/v1/library") || strings.HasPrefix(p, "/api/v1/covers/") ||
		strings.HasPrefix(p, "/api/v1/volumes/") || strings.HasPrefix(p, "/api/v1/collections") ||
		strings.HasPrefix(p, "/api/v1/screens") || p == "/api/v1/sources":
		return auth.PermLibraryView
	case p == "/logout" || p == "/" || strings.HasPrefix(p, "/anilist/") || strings.HasPrefix(p, "/ui/anilist/") || strings.HasPrefix(p, "/ui/account"):
		return "" // any signed-in user (dashboard sections gate individually)
	case strings.HasPrefix(p, "/reader/") || strings.HasPrefix(p, "/api/reader/"):
		return auth.PermReaderUse
	case strings.HasPrefix(p, "/ui/users/") && strings.HasSuffix(p, "/approve"):
		return auth.PermUsersApprove
	case (p == "/users" || strings.HasPrefix(p, "/ui/users")) && r.Method == http.MethodGet:
		return auth.PermUsersManage + "|" + auth.PermUsersApprove
	case p == "/users" || strings.HasPrefix(p, "/ui/users"):
		return auth.PermUsersManage
	case p == "/api/settings":
		return auth.PermSettingsManage // the JSON API has no per-section split
	case p == "/settings" || p == "/ui/settings":
		return ""
	case p == "/sources" || strings.HasPrefix(p, "/ui/sources"):
		return auth.PermSourcesManage
	case p == "/ui/health":
		return auth.PermServicesView
	case p == "/backups" || strings.HasPrefix(p, "/ui/backups"):
		return auth.PermSettingsManage
	case p == "/ui/sessions":
		return auth.PermSessionsView
	case strings.HasPrefix(p, "/ui/jobs/") && r.Method != http.MethodGet:
		return auth.PermJobsManage
	case strings.HasPrefix(p, "/ui/jobs/"):
		return auth.PermJobsView
	case p == "/search" || strings.HasPrefix(p, "/ui/search") || p == "/ui/library/add":
		return auth.PermLibraryAdd
	case p == "/import" || strings.HasPrefix(p, "/ui/import"):
		return auth.PermImportUse
	case strings.Contains(p, "/chapters/") && strings.HasSuffix(p, "/download"): // export CBZ/ZIP
		return auth.PermReaderUse
	case strings.Contains(p, "/chapters/") && (strings.HasSuffix(p, "/remove") || strings.HasSuffix(p, "/rename")):
		return auth.PermLibraryManage // delete from disk / edit chapter metadata
	case strings.Contains(p, "/chapters/bulk-rename") || strings.HasSuffix(p, "/chapters/reset-names") ||
		(strings.Contains(p, "/chapters/") && strings.HasSuffix(p, "/reset-name")):
		return auth.PermLibraryManage // name/volume edits mirror /rename
	case strings.HasSuffix(p, "/chapters/delete-range"):
		return auth.PermLibraryManage
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return auth.PermLibraryView
	case strings.Contains(p, "/chapters/"): // read/unread marking, bulk range
		return auth.PermReaderUse
	case strings.HasSuffix(p, "/anilist-sync"): // personal list reconcile
		return auth.PermReaderUse
	case strings.HasSuffix(p, "/favourite"): // personal curation
		return auth.PermLibraryView
	case strings.HasPrefix(p, "/ui/screens"): // personal saved views
		return auth.PermLibraryView
	case strings.HasPrefix(p, "/ui/collections") || strings.HasSuffix(p, "/collections/add"): // personal collections: create/add/remove/delete/pin
		return auth.PermLibraryView
	case strings.HasPrefix(p, "/ui/volumes/") && (strings.HasSuffix(p, "/read") || strings.HasSuffix(p, "/unread")):
		return auth.PermReaderUse
	case p == "/ui/import/attach-volumes":
		return auth.PermImportUse
	default:
		return auth.PermLibraryManage
	}
}

// canAny accepts "a|b" alternatives from requiredPerm.
func canAny(user *auth.User, perms string) bool {
	for _, p := range strings.Split(perms, "|") {
		if user.Can(p) {
			return true
		}
	}
	return false
}

func resolveUser(r *http.Request, svc *service.JobService) *auth.User {
	ctx := r.Context()
	if !authEnabled() {
		// Single-user mode: everything acts as the env admin.
		admin, _ := svc.Auth().GetUser(ctx, auth.EnvAdminID)
		return admin
	}
	// Scripted clients: a personal API token via header or bearer.
	if token := headerToken(r); token != "" {
		if user, err := svc.Auth().UserByAPIToken(ctx, token); err == nil && user != nil {
			return user
		}
	}
	if c, err := r.Cookie("kaodoku_session"); err == nil {
		if user, err := svc.Auth().UserBySession(ctx, c.Value); err == nil && user != nil {
			return user
		}
	}
	if t := r.Header.Get("X-Auth-Token"); t != "" {
		if user, err := svc.Auth().UserBySession(ctx, t); err == nil && user != nil {
			return user
		}
	}
	if c, err := r.Cookie("KOMGA-SESSION"); err == nil {
		if user, err := svc.Auth().UserBySession(ctx, c.Value); err == nil && user != nil {
			return user
		}
	}
	if username, password, ok := r.BasicAuth(); ok {
		if user := basicAuthUser(r, svc, username, password); user != nil {
			return user
		}
	}
	return nil
}

// basicCreds caches verified Basic credentials so OPDS page streams don't pay
// bcrypt per request; entries expire after basicAuthTTL.
var basicCreds = struct {
	sync.Mutex
	m map[[32]byte]basicCred
}{m: map[[32]byte]basicCred{}}

type basicCred struct {
	userID int64
	expiry time.Time
}

const basicAuthTTL = 5 * time.Minute

func basicAuthUser(r *http.Request, svc *service.JobService, username, password string) *auth.User {
	ctx := r.Context()
	key := sha256.Sum256([]byte(strings.TrimSpace(username) + "\x00" + password))
	now := time.Now()
	basicCreds.Lock()
	cred, ok := basicCreds.m[key]
	basicCreds.Unlock()
	if !ok || now.After(cred.expiry) {
		id, err := svc.Auth().Authenticate(ctx, username, password)
		if err != nil {
			return nil
		}
		cred = basicCred{userID: id, expiry: now.Add(basicAuthTTL)}
		basicCreds.Lock()
		for k, c := range basicCreds.m {
			if now.After(c.expiry) {
				delete(basicCreds.m, k)
			}
		}
		basicCreds.m[key] = cred
		basicCreds.Unlock()
	}
	user, err := svc.Auth().GetUser(ctx, cred.userID)
	if err != nil {
		return nil
	}
	return user
}

// flushBasicCreds drops all cached Basic credentials; call on password change.
func flushBasicCreds() {
	basicCreds.Lock()
	basicCreds.m = map[[32]byte]basicCred{}
	basicCreds.Unlock()
}

// clientIP prefers the proxy-forwarded address from a local proxy.
func clientIP(r *http.Request) string {
	peer := remoteIP(r.RemoteAddr)
	if trustedProxy(peer) {
		fwd := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if net.ParseIP(fwd) != nil {
			return fwd
		}
	}
	if peer != "" {
		return peer
	}
	return r.RemoteAddr
}

func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return host
	}
	if net.ParseIP(addr) != nil {
		return addr
	}
	return ""
}

func trustedProxy(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func forwardedProto(r *http.Request) string {
	if trustedProxy(remoteIP(r.RemoteAddr)) {
		return strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	}
	return ""
}

func forwardedHost(r *http.Request) string {
	if trustedProxy(remoteIP(r.RemoteAddr)) {
		return strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	}
	return ""
}

func requestHost(r *http.Request) string {
	if fwd := forwardedHost(r); fwd != "" {
		return fwd
	}
	return r.Host
}

func headerToken(r *http.Request) string {
	if t := r.Header.Get("X-API-Key"); t != "" {
		return t
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// secureRequest reports whether the request arrived over https (directly or
// via a TLS-terminating proxy) so cookies can carry the Secure flag.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || forwardedProto(r) == "https"
}

const loginPage = `<!doctype html><html lang="en" data-theme="mocha"><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in · Kaodoku</title><link rel="icon" href="/static/favicon.ico" sizes="any"><link rel="stylesheet" href="/static/app.css">
<body class="grid min-h-screen place-items-center bg-base-200">
<form class="card bg-base-100 card-body w-80 gap-3" method="post" action="/login">
<h1 class="text-xl font-bold">Kao<span class="text-primary">doku</span></h1>
%s
<input class="input w-full" name="username" placeholder="Username" autofocus autocomplete="username">
<input class="input w-full" type="password" name="password" placeholder="Password" autocomplete="current-password">
<button class="btn btn-primary w-full">Sign in</button>
<!--sso-->
</form></body></html>`

// renderLoginPage fills the notice slot and adds the SSO button when OIDC is
// configured.
func renderLoginPage(notice string) string {
	page := strings.Replace(loginPage, "%s", notice, 1)
	if oidcEnabled() {
		page = strings.Replace(page, "<!--sso-->",
			`<a class="btn btn-outline w-full" href="/auth/oidc/login">Sign in with SSO</a>`, 1)
	}
	return page
}

func registerAuthRoutes(mux *http.ServeMux, svc *service.JobService) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(renderLoginPage("")))
	})
	mux.HandleFunc("GET /auth/oidc/login", oidcLogin)
	mux.HandleFunc("GET /auth/oidc/callback", oidcCallback(svc))
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		token, err := svc.Auth().Login(r.Context(), r.FormValue("username"), r.FormValue("password"), r.UserAgent(), clientIP(r))
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(renderLoginPage(`<p class="text-sm text-error">Invalid username or password.</p>`)))
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "kaodoku_session", Value: token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: secureRequest(r),
			MaxAge: 30 * 24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("kaodoku_session"); err == nil {
			svc.Auth().Logout(r.Context(), c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "kaodoku_session", Value: "", Path: "/", MaxAge: -1})
		w.Header().Set("HX-Redirect", "/login")
	})
}
