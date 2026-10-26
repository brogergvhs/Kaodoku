package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/brogergvhs/kaodoku/internal/auth"
)

type accountView struct {
	User     *auth.User
	Sessions []auth.SessionInfo
	Tokens   []auth.APIToken
	NewToken string // freshly minted, shown once
}

func (u *webUI) accountData(r *http.Request, newToken string) accountView {
	user := userFrom(r.Context())
	token := ""
	if c, err := r.Cookie("kaodoku_session"); err == nil {
		token = c.Value
	}
	sessions, _ := u.svc.Auth().Sessions(r.Context(), user.ID, token)
	tokens, _ := u.svc.Auth().APITokens(r.Context(), user.ID)
	return accountView{User: user, Sessions: sessions, Tokens: tokens, NewToken: newToken}
}

func (u *webUI) accountFrag(w http.ResponseWriter, r *http.Request) {
	u.frag(w, "accountCard", u.accountData(r, ""))
}

func (u *webUI) accountPassword(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if err := u.svc.Auth().ChangePassword(r.Context(), user.ID, r.FormValue("current"), r.FormValue("password")); err != nil {
		u.fail(w, err)
		return
	}
	flushBasicCreds()
	u.frag(w, "toast", toastView{OK: true, Msg: "Password changed ✓"})
}

func (u *webUI) accountRevokeSessions(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	token := ""
	if c, err := r.Cookie("kaodoku_session"); err == nil {
		token = c.Value
	}
	if err := u.svc.Auth().RevokeOtherSessions(r.Context(), user.ID, token); err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "accountCard", u.accountData(r, ""))
}

func (u *webUI) accountTokenCreate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	ttlDays, _ := strconv.Atoi(r.FormValue("ttl_days"))
	token, err := u.svc.Auth().CreateAPIToken(r.Context(), user.ID, r.FormValue("name"), "", ttlDays)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "accountCard", u.accountData(r, token))
}

func (u *webUI) accountTokenDelete(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.Auth().DeleteAPIToken(r.Context(), user.ID, id); err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "accountCard", u.accountData(r, ""))
}

func (u *webUI) accountContent(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !user.Can(auth.PermUsersManage) {
		u.fail(w, fmt.Errorf("missing permission %s", auth.PermUsersManage))
		return
	}
	me, err := u.svc.Auth().GetUser(r.Context(), user.ID)
	if err != nil || me == nil {
		u.fail(w, fmt.Errorf("user not found"))
		return
	}
	if err := u.svc.Auth().UpdateUser(r.Context(), me.ID, me.RoleID, "", contentGuardsFromForm(r)); err != nil {
		u.fail(w, err)
		return
	}
	flushBasicCreds()
	u.frag(w, "toast", toastView{OK: true, Msg: "Saved ✓"})
}
