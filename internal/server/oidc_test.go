package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/brogergvhs/kaodoku/internal/auth"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that signs id_tokens with claims the test controls.
type fakeIdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/auth",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithHeader("kid", "test"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		body, _ := json.Marshal(idp.claims)
		jws, err := signer.Sign(body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		raw, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "id_token": raw,
		})
	})
	return idp
}

func TestOIDCFullFlow(t *testing.T) {
	idp := newFakeIdP(t)
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	t.Setenv("KAODOKU_OIDC_ISSUER", idp.server.URL)
	t.Setenv("KAODOKU_OIDC_CLIENT_ID", "kaodoku")
	t.Setenv("KAODOKU_OIDC_CLIENT_SECRET", "sekrit")
	t.Setenv("KAODOKU_OIDC_REDIRECT_URL", "http://example.com/auth/oidc/callback")
	t.Setenv("KAODOKU_OIDC_AUTO_PROVISION", "true")
	t.Setenv("KAODOKU_OIDC_ROLE_CLAIM", "groups")
	t.Setenv("KAODOKU_OIDC_ROLE_MAP", "staff:admin,folk:member")

	api, svc, _, _ := opdsTestAPI(t)

	begin := func() (state, nonce, cookie string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("login redirect = %d; %s", rec.Code, rec.Body.String())
		}
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || !strings.HasPrefix(loc.String(), idp.server.URL+"/auth") {
			t.Fatalf("authorize url = %q", rec.Header().Get("Location"))
		}
		var raw string
		for _, c := range rec.Result().Cookies() {
			if c.Name == oidcStateCookie {
				raw = c.Value
			}
		}
		state, nonce = loc.Query().Get("state"), loc.Query().Get("nonce")
		if state == "" || nonce == "" || raw != state+"."+nonce {
			t.Fatalf("state/nonce wiring: %q %q cookie=%q", state, nonce, raw)
		}
		return state, nonce, raw
	}

	finish := func(state, cookie string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state="+state, nil)
		req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookie})
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}

	sub := "user-12345"
	state, nonce, cookie := begin()
	idp.claims = map[string]any{
		"iss": idp.server.URL, "aud": "kaodoku", "sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce, "preferred_username": "sso.reader", "groups": []string{"staff"},
	}
	rec := finish(state, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	var session string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kaodoku_session" {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie issued")
	}
	user, err := svc.Auth().UserBySession(t.Context(), session)
	if err != nil || user == nil {
		t.Fatalf("session resolve: %v %v", user, err)
	}
	if user.Username != "sso.reader" || user.RoleName != "admin" || user.Origin != "oidc" || user.OIDCSubject != sub {
		t.Fatalf("provisioned user = %+v", user)
	}

	// Second login, same subject, downgraded group: same user, role re-mapped.
	state, nonce, cookie = begin()
	idp.claims["nonce"] = nonce
	idp.claims["groups"] = []string{"folk"}
	if rec := finish(state, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("relogin = %d", rec.Code)
	}
	again, err := svc.Auth().UserByOIDCSubject(t.Context(), sub)
	if err != nil || again == nil || again.ID != user.ID {
		t.Fatalf("relogin must reuse the user: %+v %v", again, err)
	}
	if again.RoleName != "member" {
		t.Fatalf("role not synced from claim: %q", again.RoleName)
	}

	// Removed from every mapped group: role revoked to the default.
	state, nonce, cookie = begin()
	idp.claims["nonce"] = nonce
	idp.claims["groups"] = []string{"unmapped-team"}
	if rec := finish(state, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("revocation login = %d", rec.Code)
	}
	revoked, _ := svc.Auth().UserByOIDCSubject(t.Context(), sub)
	if revoked.RoleName != "member" {
		t.Fatalf("role not revoked to default: %q", revoked.RoleName)
	}

	// Tampered state is rejected.
	_, nonce, cookie = begin()
	idp.claims["nonce"] = nonce
	if rec := finish("forged-state", cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d", rec.Code)
	}
	// Wrong-nonce token is rejected even with a valid state.
	state, _, cookie = begin()
	idp.claims["nonce"] = "stale-nonce"
	if rec := finish(state, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale nonce = %d", rec.Code)
	}

	// OIDC users cannot password-login (empty hash refuses).
	if _, err := svc.Auth().Login(t.Context(), "sso.reader", "anything", "", ""); err == nil {
		t.Fatal("password login must fail for OIDC users")
	}
}

func TestOIDCUnlinkedWithoutProvisioning(t *testing.T) {
	idp := newFakeIdP(t)
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	t.Setenv("KAODOKU_OIDC_ISSUER", idp.server.URL)
	t.Setenv("KAODOKU_OIDC_CLIENT_ID", "kaodoku")
	t.Setenv("KAODOKU_OIDC_REDIRECT_URL", "http://example.com/auth/oidc/callback")

	api, svc, _, _ := opdsTestAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state, nonce := loc.Query().Get("state"), loc.Query().Get("nonce")

	idp.claims = map[string]any{
		"iss": idp.server.URL, "aud": "kaodoku", "sub": "stranger",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce,
	}
	req = httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state + "." + nonce})
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not linked") {
		t.Fatalf("unlinked subject = %d %s", rec.Code, rec.Body.String())
	}

	// Admin links the subject, then SSO signs in as that user.
	ctx := t.Context()
	roles, err := svc.Auth().ListRoles(ctx)
	if err != nil || len(roles) == 0 {
		t.Fatal(err)
	}
	if err := svc.Auth().CreateUser(ctx, "linked.user", "pass1234", roles[0].ID, auth.ContentGuards{}); err != nil {
		t.Fatal(err)
	}
	uid, err := svc.Auth().Authenticate(ctx, "linked.user", "pass1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Auth().SetOIDCSubject(ctx, uid, "stranger"); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	state, nonce = loc.Query().Get("state"), loc.Query().Get("nonce")
	idp.claims["nonce"] = nonce
	req = httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state + "." + nonce})
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("linked login = %d %s", rec.Code, rec.Body.String())
	}
	var session string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kaodoku_session" {
			session = c.Value
		}
	}
	user, err := svc.Auth().UserBySession(ctx, session)
	if err != nil || user == nil || user.ID != uid || user.Username != "linked.user" {
		t.Fatalf("linked session user = %+v %v", user, err)
	}
}

func TestOIDCApprovalFlow(t *testing.T) {
	idp := newFakeIdP(t)
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	t.Setenv("KAODOKU_OIDC_ISSUER", idp.server.URL)
	t.Setenv("KAODOKU_OIDC_CLIENT_ID", "kaodoku")
	t.Setenv("KAODOKU_OIDC_REDIRECT_URL", "http://example.com/auth/oidc/callback")
	t.Setenv("KAODOKU_OIDC_AUTO_PROVISION", "true")
	t.Setenv("KAODOKU_OIDC_REQUIRE_APPROVAL", "true")

	api, svc, _, _ := opdsTestAPI(t)
	ctx := t.Context()

	sso := func(sub string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil)
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		loc, _ := url.Parse(rec.Header().Get("Location"))
		state, nonce := loc.Query().Get("state"), loc.Query().Get("nonce")
		idp.claims = map[string]any{
			"iss": idp.server.URL, "aud": "kaodoku", "sub": sub,
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"nonce": nonce, "preferred_username": "newbie",
		}
		req = httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state="+state, nil)
		req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state + "." + nonce})
		rec = httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}

	rec := sso("pending-sub")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "approval") {
		t.Fatalf("pending signup = %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kaodoku_session" && c.Value != "" {
			t.Fatal("pending user must not get a session")
		}
	}
	user, err := svc.Auth().UserByOIDCSubject(ctx, "pending-sub")
	if err != nil || user == nil || !user.Pending {
		t.Fatalf("provisioned pending user = %+v %v", user, err)
	}

	// Even a directly-minted session is blocked at the middleware.
	session, err := svc.Auth().IssueSession(ctx, user.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/library", nil)
	req.AddCookie(&http.Cookie{Name: "kaodoku_session", Value: session})
	blocked := httptest.NewRecorder()
	api.ServeHTTP(blocked, req)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("pending session must be blocked, got %d", blocked.Code)
	}

	// Admin approves through the endpoint (users.approve via the admin role).
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/ui/users/%d/approve", user.ID), nil)
	req.SetBasicAuth("boss", "secret123")
	approve := httptest.NewRecorder()
	api.ServeHTTP(approve, req)
	if approve.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", approve.Code, approve.Body.String())
	}

	// Second SSO login now mints a session.
	rec = sso("pending-sub")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("post-approval login = %d %s", rec.Code, rec.Body.String())
	}
	var got string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kaodoku_session" {
			got = c.Value
		}
	}
	if got == "" {
		t.Fatal("approved user must get a session")
	}
}
