package server

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/service"
)

// trackerRedirectURL keeps AniList on its original callback path so existing apps stay valid.
func trackerRedirectURL(r *http.Request, provider string) string {
	scheme := "http"
	if r.TLS != nil || forwardedProto(r) == "https" {
		scheme = "https"
	}
	path := "/trackers/" + provider + "/callback"
	if provider == catalog.AniListProvider {
		path = "/anilist/callback"
	}
	return fmt.Sprintf("%s://%s%s", scheme, requestHost(r), path)
}

func pathProvider(r *http.Request) string {
	if p := r.PathValue("provider"); p != "" {
		return p
	}
	return catalog.AniListProvider
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func (u *webUI) trackerConnect(w http.ResponseWriter, r *http.Request) {
	provider := pathProvider(r)
	p, err := u.svc.Provider(r.Context(), provider)
	o, ok := p.(catalog.OAuth)
	if err != nil || !ok || u.svc.TrackerLogin(r.Context(), provider) != "oauth" {
		http.Error(w, "this tracker is not configured yet (settings → Metadata & tracking)", http.StatusBadRequest)
		return
	}
	state, verifier := randomHex(16), randomHex(48)
	for name, value := range map[string]string{"tracker_state": provider + ":" + state, "tracker_verifier": verifier} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	http.Redirect(w, r, o.AuthorizeURL(u.svc.TrackerApp(r.Context(), provider, trackerRedirectURL(r, provider)), state, verifier), http.StatusSeeOther)
}

func (u *webUI) trackerCallback(w http.ResponseWriter, r *http.Request) {
	provider := pathProvider(r)
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, fmt.Sprintf("%s refused the connection: %s %s", u.svc.ProviderLabel(provider), e, r.URL.Query().Get("error_description")), http.StatusBadRequest)
		return
	}
	if c, err := r.Cookie("tracker_state"); err != nil || c.Value != provider+":"+r.URL.Query().Get("state") {
		http.Error(w, "state mismatch — restart the connection from settings", http.StatusBadRequest)
		return
	}
	verifier := ""
	if c, err := r.Cookie("tracker_verifier"); err == nil {
		verifier = c.Value
	}
	for _, name := range []string{"tracker_state", "tracker_verifier"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "the tracker did not return an authorization code", http.StatusBadRequest)
		return
	}
	if err := u.svc.ConnectOAuth(r.Context(), auth.UserID(r.Context()), provider, trackerRedirectURL(r, provider), code, verifier); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	u.kick()
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

func (u *webUI) trackerLogin(w http.ResponseWriter, r *http.Request) {
	if err := u.svc.LoginTracker(r.Context(), auth.UserID(r.Context()), pathProvider(r), r.FormValue("username"), r.FormValue("password")); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	w.Header().Set("HX-Refresh", "true")
}

func (u *webUI) trackerDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := u.svc.DisconnectTracker(r.Context(), auth.UserID(r.Context()), pathProvider(r)); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (u *webUI) trackerSyncNow(w http.ResponseWriter, r *http.Request) {
	if err := u.svc.EnqueueTrackerSync(r.Context(), auth.UserID(r.Context()), pathProvider(r)); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	u.frag(w, "toast", toastView{OK: true, Msg: "Sync queued ✓"})
}

func (u *webUI) trackerPrefs(w http.ResponseWriter, r *http.Request) {
	provider := pathProvider(r)
	for _, a := range u.svc.TrackerAccounts(r.Context(), auth.UserID(r.Context())) {
		if a.Provider == provider {
			u.frag(w, "trackerPrefs", a)
			return
		}
	}
	u.fail(w, fmt.Errorf("no %s account connected", u.svc.ProviderLabel(provider)))
}

type unlinkedView struct {
	Provider, Label string
	Titles          []service.UnlinkedTitle
}

func (u *webUI) unlinked(r *http.Request, provider string) ([]service.UnlinkedTitle, error) {
	titles, err := u.svc.UnlinkedTitles(r.Context(), provider)
	return slices.DeleteFunc(titles, func(t service.UnlinkedTitle) bool { return !contentAllowed(r.Context(), t.IsAdult, t.ContentTags) }), err
}

func (u *webUI) trackerUnlinked(w http.ResponseWriter, r *http.Request) {
	if !userFrom(r.Context()).Can(auth.PermLibraryManage) {
		writeError(w, http.StatusForbidden, "linking titles needs the library manage permission")
		return
	}
	provider := pathProvider(r)
	titles, err := u.unlinked(r, provider)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "trackerUnlinked", unlinkedView{Provider: provider, Label: u.svc.ProviderLabel(provider), Titles: titles})
}

type mangaBakaView struct {
	Provider, Label    string
	Links, Suggestions []service.MangaBakaLink
}

func (v mangaBakaView) Total() int { return len(v.Links) + len(v.Suggestions) }

func (u *webUI) mangaBakaLinks(r *http.Request, provider string) mangaBakaView {
	links, suggestions, _ := u.svc.MangaBakaLinks(r.Context(), provider)
	allowed := func(l service.MangaBakaLink) bool { return !contentAllowed(r.Context(), l.IsAdult, l.ContentTags) }
	return mangaBakaView{provider, u.svc.ProviderLabel(provider), slices.DeleteFunc(links, allowed), slices.DeleteFunc(suggestions, allowed)}
}

func (u *webUI) trackerMangaBaka(w http.ResponseWriter, r *http.Request) {
	if !userFrom(r.Context()).Can(auth.PermLibraryManage) {
		writeError(w, http.StatusForbidden, "linking titles needs the library manage permission")
		return
	}
	provider := pathProvider(r)
	if r.Method == http.MethodPost {
		id, _ := strconv.ParseInt(r.FormValue("title"), 10, 64)
		var err error
		switch action := r.FormValue("action"); {
		case action == "confirm" && id == 0:
			for _, l := range u.mangaBakaLinks(r, provider).Links {
				if err = u.svc.ConfirmMangaBakaLink(r.Context(), l.Title.ID, provider); err != nil {
					break
				}
			}
		case action == "confirm":
			err = u.svc.ConfirmMangaBakaLink(r.Context(), id, provider)
		default:
			err = u.svc.AnswerMangaBakaSuggestion(r.Context(), id, provider, action == "use")
		}
		if err != nil {
			u.fail(w, err)
			return
		}
	}
	u.frag(w, "trackerMangaBaka", u.mangaBakaLinks(r, provider))
}

type missingView struct {
	Provider, Label string
	Titles          []service.MissingRating
}

func (u *webUI) missingRatings(r *http.Request, provider string) missingView {
	titles, _ := u.svc.MissingRatings(r.Context(), provider)
	titles = slices.DeleteFunc(titles, func(t service.MissingRating) bool { return !contentAllowed(r.Context(), t.IsAdult, t.ContentTags) })
	return missingView{provider, u.svc.ProviderLabel(provider), titles}
}

func (u *webUI) trackerMissingRatings(w http.ResponseWriter, r *http.Request) {
	provider := pathProvider(r)
	if r.Method == http.MethodPost {
		for _, t := range u.missingRatings(r, provider).Titles {
			if err := u.svc.AnswerMissingRating(r.Context(), t.ID, provider, r.FormValue("action") == "send"); err != nil {
				u.fail(w, err)
				return
			}
		}
	}
	u.frag(w, "trackerMissingRatings", u.missingRatings(r, provider))
}

type malXMLView struct {
	Step                       string
	Exported, Unlinked, Absent int
	CanLink                    bool
	Result                     *service.ListImportResult
	Error                      string
}

func (u *webUI) malXML(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view := malXMLView{Step: r.URL.Query().Get("step")}
	if r.Method == http.MethodPost {
		file, _, err := r.FormFile("file")
		if err == nil {
			defer file.Close()
			opts := service.ListImport{Progress: r.FormValue("progress") != "", Ratings: r.FormValue("ratings") != "", Overwrite: r.FormValue("ratings") == "all", AddMissing: r.FormValue("add") != ""}
			res, ierr := u.svc.ImportMALXML(ctx, file, opts, r.FormValue("apply") != "")
			view.Result, err = &res, ierr
		}
		if err != nil {
			view.Error = err.Error()
		}
		u.kick()
		u.frag(w, "malXMLResult", view)
		return
	}
	if view.Step == "export" {
		view.Exported, view.Unlinked, view.Absent, _ = u.svc.ExportMALXML(ctx, io.Discard)
		view.CanLink = userFrom(ctx).Can(auth.PermLibraryManage) && slices.ContainsFunc(u.svc.Trackers(ctx), func(p catalog.Provider) bool { return p.Name() == catalog.MALProvider })
	}
	u.frag(w, "malXML", view)
}

func (u *webUI) malXMLExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Disposition", `attachment; filename="kaodoku-mal.xml"`)
	if _, _, _, err := u.svc.ExportMALXML(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *webUI) trackerPrefsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	on := func(k string) bool { v := r.Form[k]; return len(v) > 0 && v[len(v)-1] == "true" }
	source := service.SourceTracker
	if on("kaodoku_leads") {
		source = service.SourceKaodoku
	}
	if err := u.svc.SetTrackerPrefs(r.Context(), pathProvider(r), service.TrackerPrefs{
		AutoLink: on("auto_link"), AutoProgressive: on("auto_progressive"), Relations: on("relations"), Recommendations: on("recommendations"),
		RatingSync: r.FormValue("rating_sync"), Source: source,
	}); err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "toast", toastView{OK: true, Msg: "Saved ✓"})
	view := u.trackerCards(r)
	view.OOB = true
	u.frag(w, "trackerRows", view)
}

func (u *webUI) trackerReorder(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if err := u.svc.ReorderTrackers(r.Context(), r.Form["order"]); err != nil {
		u.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (u *webUI) trackerSuggestions(w http.ResponseWriter, r *http.Request) {
	view := u.mangaResultsView(r.Context(), "", resultView(r), u.svc.TrackerSuggestions(r.Context(), r.FormValue("provider")))
	missing := view.Items[:0]
	for _, it := range view.Items {
		if it.TitleID == 0 {
			missing = append(missing, it)
		}
	}
	view.Items = missing
	u.frag(w, "trackerSuggestions", view)
}

func (u *webUI) trackerSyncTitle(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	provider := r.FormValue("provider")
	if err := u.svc.SyncTrackersTitle(r.Context(), id, provider); err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "toast", toastView{OK: true, Msg: u.svc.ProviderLabel(provider) + " synced ✓"})
}

func (u *webUI) titleProgressiveSync(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil || !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	if err := u.svc.SetProgressiveSync(r.Context(), id, r.FormValue("provider"), r.FormValue("on") == "1"); err != nil {
		u.fail(w, err)
		return
	}
	u.titleProviders(w, r)
}

type trackerCardView struct {
	Provider  string
	Label     string
	Login     string
	Outage    string
	Account   *service.TrackerAccount
	Unlinked  int
	Missing   int
	MangaBaka int // MangaBaka links and suggestions to check
}

type trackersView struct {
	Cards      []trackerCardView
	CanConnect bool
	CanManage  bool
	Reorder    bool
	OOB        bool
}

func (u *webUI) trackerCards(r *http.Request) trackersView {
	accounts, leaders := map[string]service.TrackerAccount{}, 0
	for _, a := range u.svc.TrackerAccounts(r.Context(), auth.UserID(r.Context())) {
		accounts[a.Provider] = a
		if a.Source == service.SourceTracker {
			leaders++
		}
	}
	out := trackersView{CanManage: userFrom(r.Context()).Can(auth.PermLibraryManage), Reorder: leaders >= 2}
	for _, p := range u.svc.Trackers(r.Context()) {
		card := trackerCardView{Provider: p.Name(), Label: p.Label(), Login: u.svc.TrackerLogin(r.Context(), p.Name()), Outage: u.svc.ProviderOutage(p.Name())}
		if a, ok := accounts[p.Name()]; ok {
			card.Account = &a
			if out.CanManage {
				titles, _ := u.unlinked(r, p.Name())
				card.Unlinked = len(titles)
				card.MangaBaka = u.mangaBakaLinks(r, p.Name()).Total()
			}
			if a.Source == service.SourceTracker {
				card.Missing = len(u.missingRatings(r, p.Name()).Titles)
			}
		}
		out.Cards = append(out.Cards, card)
		out.CanConnect = out.CanConnect || card.Account == nil && card.Login != ""
	}
	slices.SortStableFunc(out.Cards, func(a, b trackerCardView) int {
		position := func(c trackerCardView) int {
			if c.Account == nil {
				return math.MaxInt
			}
			return c.Account.Position
		}
		return cmp.Compare(position(a), position(b))
	})
	return out
}

type providerPanelView struct {
	TitleID   int64
	CanManage bool
	Open      bool
	Query     string
	Links     []service.ProviderLink
}

type providerPickerView struct {
	TitleID  int64
	Provider string
	Current  string
	Results  []catalog.Manga
	Error    string
	Unlinked bool
}

func (u *webUI) titleProviders(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil || !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	links, err := u.svc.TitleLinks(r.Context(), id)
	title, terr := u.svc.GetTitle(r.Context(), id)
	if err != nil || terr != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	u.frag(w, "providerPanel", providerPanelView{TitleID: id, CanManage: userFrom(r.Context()).Can(auth.PermLibraryManage), Open: r.Method == http.MethodPost, Query: title.DisplayTitle, Links: links})
}

func (u *webUI) titleProviderSearch(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		http.NotFound(w, r)
		return
	}
	view := providerPickerView{TitleID: id, Provider: r.FormValue("provider"), Unlinked: r.FormValue("unlinked") != ""}
	query := cmp.Or(strings.TrimSpace(r.FormValue("q")), title.DisplayTitle)
	ctx := catalog.WithReason(r.Context(), "Link search")
	items, _, err := u.svc.SearchManga(ctx, view.Provider, query, 8, catalog.SearchFilter{})
	if err != nil {
		view.Error = err.Error()
	}
	view.Current = u.svc.TitleLinkedID(r.Context(), id, view.Provider)
	if view.Current != "" && !slices.ContainsFunc(items, func(m catalog.Manga) bool { return m.ProviderID == view.Current }) {
		if m, err := u.svc.CatalogManga(ctx, view.Provider, view.Current); err == nil {
			items = append([]catalog.Manga{m}, items...)
		}
	}
	view.Results = allowedManga(r.Context(), items)
	u.frag(w, "providerPicker", view)
}

func (u *webUI) titleProviderSet(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	provider, pid := r.FormValue("provider"), strings.TrimSpace(r.FormValue("provider_id"))
	if r.FormValue("mode") == "primary" || provider == u.svc.MetadataProvider(r.Context(), id) {
		if err := u.svc.SwitchTitleMetadata(r.Context(), id, provider, pid); err != nil {
			u.fail(w, err)
			return
		}
		w.Header().Set("HX-Refresh", "true")
		return
	}
	if err := u.svc.LinkExternalID(r.Context(), id, provider, pid); err != nil {
		u.fail(w, err)
		return
	}
	if r.FormValue("unlinked") != "" {
		titles, _ := u.unlinked(r, provider)
		u.frag(w, "unlinkedLeft", unlinkedView{Provider: provider, Label: u.svc.ProviderLabel(provider), Titles: titles})
		return
	}
	u.titleProviders(w, r)
}
