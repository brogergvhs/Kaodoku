package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
)

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid json")
		return false
	}
	return true
}

func (a *apiV1) guardedTitleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathID(r)
	if err != nil || !titleAllowed(r.Context(), a.svc, id) {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return 0, false
	}
	return id, true
}

// guardedChapterID resolves a chapter id and enforces the content guard on its
// title, so read-state routes can't mutate or leak restricted chapters.
func (a *apiV1) guardedChapterID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid chapter id")
		return 0, false
	}
	status, err := a.svc.ChapterReadStatus(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), a.svc, status.TitleID) {
		v1err(w, http.StatusNotFound, "not_found", "chapter not found")
		return 0, false
	}
	return id, true
}

func (a *apiV1) setFavourite(want bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.guardedTitleID(w, r)
		if !ok {
			return
		}
		t, err := a.svc.GetTitle(r.Context(), id)
		if err != nil {
			v1err(w, http.StatusNotFound, "not_found", "title not found")
			return
		}
		if t.Favourite != want {
			if _, err := a.svc.ToggleFavourite(r.Context(), id); err != nil {
				v1err(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]bool{"favourite": want})
	}
}

func (a *apiV1) patchTitle(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	var body struct {
		Monitored       *bool   `json:"monitored"`
		LanguageMode    *string `json:"language_mode"`
		RefreshInterval *string `json:"refresh_interval"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Monitored != nil {
		if err := a.svc.SetMonitored(r.Context(), id, *body.Monitored); err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	if body.LanguageMode != nil {
		if err := a.svc.SetLanguageMode(r.Context(), id, *body.LanguageMode); err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	if body.RefreshInterval != nil {
		if err := a.svc.SetRefreshInterval(r.Context(), id, *body.RefreshInterval); err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	t, err := a.svc.GetTitle(r.Context(), id)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTitleDTO(t))
}

func (a *apiV1) deleteTitle(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	deleteFiles := r.URL.Query().Get("delete_files") == "1"
	deleteAniList := r.URL.Query().Get("delete_anilist") == "1"
	if _, err := a.svc.RemoveTitleFiles(r.Context(), id, deleteFiles, deleteAniList); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiV1) volumeSetRead(read bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
			return
		}
		vol, err := a.svc.GetVolume(r.Context(), id)
		if err != nil || !titleAllowed(r.Context(), a.svc, vol.TitleID) {
			v1err(w, http.StatusNotFound, "not_found", "volume not found")
			return
		}
		if err := a.svc.SetVolumeRead(r.Context(), id, read); err != nil {
			v1err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		vol, _ = a.svc.GetVolume(r.Context(), id)
		writeJSON(w, http.StatusOK, toVolumeDTO(vol))
	}
}

func (a *apiV1) volumesReadRange(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	var body struct {
		From float64 `json:"from"`
		To   float64 `json:"to"`
		Read bool    `json:"read"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	n, err := a.svc.SetVolumeRangeRead(r.Context(), id, body.From, body.To, body.Read)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"volumes": n})
}

func (a *apiV1) ownsCollection(r *http.Request, id int64) bool {
	cols, err := a.svc.CustomCollections(r.Context())
	if err != nil {
		return false
	}
	for _, c := range cols {
		if c.ID == id {
			return true
		}
	}
	return false
}

func (a *apiV1) collectionsList(w http.ResponseWriter, r *http.Request) {
	author, smart, custom, _, err := buildCollections(r.Context(), a.svc)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	pins, _ := a.svc.SmartPins(r.Context())
	group := func(cols []collection) []collectionDTO {
		out := make([]collectionDTO, 0, len(cols))
		for _, c := range withMemberStats(r.Context(), a.svc, cols) {
			out = append(out, toCollectionDTO(c, pins))
		}
		return out
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"author": group(author), "smart": group(smart), "custom": group(custom),
	})
}

func (a *apiV1) collectionCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	id, err := a.svc.CreateCollection(r.Context(), body.Name)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, collectionDTO{ID: id, Name: body.Name})
}

func (a *apiV1) collectionGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	_, _, custom, _, err := buildCollections(r.Context(), a.svc)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for _, c := range custom {
		if c.CustomID == id {
			writeJSON(w, http.StatusOK, toCollectionDTO(withMemberStats(r.Context(), a.svc, []collection{c})[0], nil))
			return
		}
	}
	v1err(w, http.StatusNotFound, "not_found", "collection not found")
}

func (a *apiV1) collectionPatch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !a.ownsCollection(r, id) {
		v1err(w, http.StatusNotFound, "not_found", "collection not found")
		return
	}
	if err := a.svc.RenameCollection(r.Context(), id, body.Name); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, collectionDTO{ID: id, Name: body.Name})
}

func (a *apiV1) collectionDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	if !a.ownsCollection(r, id) {
		v1err(w, http.StatusNotFound, "not_found", "collection not found")
		return
	}
	if err := a.svc.DeleteCollection(r.Context(), id); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiV1) collectionMember(add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r, "id")
		titleID, err2 := parseInt64Path(r, "titleId")
		if err != nil || err2 != nil {
			v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
			return
		}
		if !titleAllowed(r.Context(), a.svc, titleID) {
			v1err(w, http.StatusNotFound, "not_found", "title not found")
			return
		}
		if !a.ownsCollection(r, id) {
			v1err(w, http.StatusNotFound, "not_found", "collection not found")
			return
		}
		fn := a.svc.RemoveFromCollection
		if add {
			fn = a.svc.AddToCollection
		}
		if err := fn(r.Context(), id, titleID); err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *apiV1) smartPin(add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		titleID, err := parseInt64Path(r, "titleId")
		if key == "" || err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", "invalid key or id")
			return
		}
		if !titleAllowed(r.Context(), a.svc, titleID) {
			v1err(w, http.StatusNotFound, "not_found", "title not found")
			return
		}
		fn := a.svc.RemoveSmartPin
		if add {
			fn = a.svc.PinToSmart
		}
		if err := fn(r.Context(), key, titleID); err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *apiV1) screensList(w http.ResponseWriter, r *http.Request) {
	screens, err := a.svc.Screens(r.Context())
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	items := make([]screenDTO, 0, len(screens))
	for _, s := range screens {
		items = append(items, toScreenDTO(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *apiV1) screenSave(w http.ResponseWriter, r *http.Request) {
	var body screenDTO
	if !decodeBody(w, r, &body) {
		return
	}
	body.ID = 0
	if r.Method == http.MethodPatch {
		id, err := pathID(r)
		if err != nil {
			v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
			return
		}
		if _, err := a.svc.GetScreen(r.Context(), id); err != nil {
			v1err(w, http.StatusNotFound, "not_found", "screen not found")
			return
		}
		body.ID = id
	}
	id, err := a.svc.SaveScreen(r.Context(), library.Screen{ID: body.ID, Name: body.Name, Config: body.Config})
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body.ID = id
	writeJSON(w, http.StatusOK, body)
}

func (a *apiV1) screenDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	if _, err := a.svc.GetScreen(r.Context(), id); err != nil {
		v1err(w, http.StatusNotFound, "not_found", "screen not found")
		return
	}
	if err := a.svc.DeleteScreen(r.Context(), id); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiV1) screensReorder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.svc.ReorderScreens(r.Context(), body.IDs); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiV1) meSettingsGet(w http.ResponseWriter, r *http.Request) {
	stored := a.svc.UserSettings(r.Context(), userFrom(r.Context()).ID)
	out := userSettingsDTO{
		ReaderMode: stored["reader.mode"], ReaderDir: stored["reader.dir"],
		ReaderFit: stored["reader.fit"], Theme: stored[service.SettingUITheme],
		ReaderPageLayout:   stored["reader.page_layout"],
		ReaderSplitWide:    stored["reader.split_wide"] == "true",
		ReaderImageQuality: stored["reader.image_quality"],
	}
	if z, err := strconv.ParseFloat(stored["reader.zoom"], 64); err == nil {
		out.ReaderZoom = &z
	}
	writeJSON(w, http.StatusOK, out)
}

// meSettingsPut validates everything before writing so a bad field can't leave
// a half-applied update; empty values clear the stored setting.
func (a *apiV1) meSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ReaderMode         *string  `json:"reader_mode"`
		ReaderDir          *string  `json:"reader_dir"`
		ReaderFit          *string  `json:"reader_fit"`
		ReaderZoom         *float64 `json:"reader_zoom"`
		ReaderPageLayout   *string  `json:"reader_page_layout"`
		ReaderSplitWide    *bool    `json:"reader_split_wide"`
		ReaderImageQuality *string  `json:"reader_image_quality"`
		Theme              *string  `json:"theme"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	writes := map[string]string{}
	if body.ReaderMode != nil {
		writes["reader.mode"] = *body.ReaderMode
	}
	if body.ReaderDir != nil {
		writes["reader.dir"] = *body.ReaderDir
	}
	if body.ReaderFit != nil {
		writes["reader.fit"] = *body.ReaderFit
	}
	if body.ReaderZoom != nil {
		writes["reader.zoom"] = strconv.FormatFloat(*body.ReaderZoom, 'f', -1, 64)
	}
	if body.ReaderPageLayout != nil {
		writes["reader.page_layout"] = *body.ReaderPageLayout
	}
	if body.ReaderSplitWide != nil {
		writes["reader.split_wide"] = strconv.FormatBool(*body.ReaderSplitWide)
	}
	if body.ReaderImageQuality != nil {
		writes["reader.image_quality"] = *body.ReaderImageQuality
	}
	if body.Theme != nil {
		if *body.Theme != "" {
			if err := service.ValidateSetting(service.SettingUITheme, *body.Theme); err != nil {
				v1err(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
		}
		writes[service.SettingUITheme] = *body.Theme
	}
	userID := userFrom(r.Context()).ID
	for key, value := range writes {
		if err := a.svc.SetUserSetting(r.Context(), userID, key, value); err != nil {
			v1err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	a.meSettingsGet(w, r)
}

func (a *apiV1) trackers(r *http.Request) []trackerDTO {
	var out []trackerDTO
	for _, c := range (&webUI{svc: a.svc}).trackerCards(r).Cards {
		d := trackerDTO{Provider: c.Provider, Label: c.Label, Login: c.Login, Outage: c.Outage}
		if c.Account != nil {
			d.Connected, d.Name, d.ExpiresAt, d.LastError = true, c.Account.Name, c.Account.ExpiresAt, c.Account.LastError
		}
		out = append(out, d)
	}
	return out
}

func (a *apiV1) aniListStatus(r *http.Request) aniListDTO {
	for _, t := range a.svc.TrackerAccounts(r.Context(), userFrom(r.Context()).ID) {
		if t.Provider == catalog.AniListProvider {
			return aniListDTO{Connected: true, Name: t.Name, ExpiresAt: t.ExpiresAt}
		}
	}
	return aniListDTO{}
}

func (a *apiV1) anilistStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.aniListStatus(r))
}

func (a *apiV1) trackersList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": a.trackers(r), "default": a.svc.DefaultProvider(r.Context())})
}

func (a *apiV1) trackerLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.svc.LoginTracker(r.Context(), userFrom(r.Context()).ID, r.PathValue("provider"), body.Username, body.Password); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *apiV1) anilistSync(w http.ResponseWriter, r *http.Request) {
	a.syncTrackers(w, r, "")
}

func (a *apiV1) trackerSync(w http.ResponseWriter, r *http.Request) {
	a.syncTrackers(w, r, r.PathValue("provider"))
}

func (a *apiV1) syncTrackers(w http.ResponseWriter, r *http.Request, provider string) {
	var body struct {
		TitleID int64 `json:"title_id"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeBody(w, r, &body) {
		return
	}
	var err error
	if body.TitleID > 0 {
		if !titleAllowed(r.Context(), a.svc, body.TitleID) {
			v1err(w, http.StatusNotFound, "not_found", "title not found")
			return
		}
		err = a.svc.SyncTrackersTitle(r.Context(), body.TitleID, provider)
	} else {
		err = a.svc.EnqueueTrackerSync(r.Context(), userFrom(r.Context()).ID, provider)
	}
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *apiV1) anilistDisconnect(w http.ResponseWriter, r *http.Request) {
	a.disconnectTracker(w, r, catalog.AniListProvider)
}

func (a *apiV1) trackerDisconnect(w http.ResponseWriter, r *http.Request) {
	a.disconnectTracker(w, r, r.PathValue("provider"))
}

func (a *apiV1) disconnectTracker(w http.ResponseWriter, r *http.Request, provider string) {
	if err := a.svc.DisconnectTracker(r.Context(), userFrom(r.Context()).ID, provider); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// searchItemDTO is a manga plus its library title id when already tracked.
type searchItemDTO struct {
	catalog.Manga
	TitleID int64 `json:"title_id,omitempty"`
}

func toSearchItems(views []searchResultView) []searchItemDTO {
	out := make([]searchItemDTO, 0, len(views))
	for _, v := range views {
		out = append(out, searchItemDTO{Manga: v.Manga, TitleID: v.TitleID})
	}
	return out
}

// wantedSearch mirrors the web search: relevance or POPULARITY_DESC browse,
// mixed include/exclude tags split by kind, and page scanning so guard-filtered
// pages don't come back empty while more results exist.
func (a *apiV1) wantedSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	term := strings.TrimSpace(q.Get("q"))
	include, exclude := splitCSV(q.Get("include_tags")), splitCSV(q.Get("exclude_tags"))
	filter := catalog.SearchFilter{Sort: anilistSort(q.Get("sort"), q.Get("dir"))}
	if term == "" && filter.Sort == "" {
		filter.Sort = "POPULARITY_DESC"
	}
	if options, err := a.svc.ProviderTags(r.Context(), cmp.Or(q.Get("provider"), a.svc.DefaultProvider(r.Context()))); err == nil && len(options) > 0 {
		filter.GenreIn, filter.TagIn = splitByKind(include, options)
		filter.GenreNotIn, filter.TagNotIn = splitByKind(exclude, options)
		excludeBlocked(r.Context(), &filter, options)
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	var views []searchResultView
	more, last := false, page
	for scan := 0; scan < 3; scan++ {
		last = page + scan
		filter.Page = last
		fetched, m, err := a.svc.SearchManga(r.Context(), q.Get("provider"), term, searchPerPage, filter)
		if err != nil {
			v1err(w, http.StatusBadGateway, "unavailable", err.Error())
			return
		}
		views, more = stripItems(r.Context(), a.svc, fetched), m
		if len(views) > 0 || !more {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": toSearchItems(views), "has_more": more, "page": last})
}

// wantedTrending mirrors the web: personalized recommendations, falling back to
// a guarded popularity browse so the grid is never empty.
func (a *apiV1) wantedTrending(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.RecommendedManga(r.Context(), 18)
	if err != nil {
		items = nil
	}
	views := stripItems(r.Context(), a.svc, items)
	if len(views) > 18 {
		views = views[:18]
	}
	if len(views) == 0 {
		views = stripItems(r.Context(), a.svc, guardedBrowseManga(r.Context(), a.svc, "", "", ""))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": toSearchItems(views)})
}

// libraryAdd is the web's one-click add: track a catalog title and kick the
// job runner so matching/downloading starts immediately.
func (a *apiV1) libraryAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider   string      `json:"provider"`
		ProviderID json.Number `json:"provider_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	title, err := a.svc.AddCatalogTitle(r.Context(), body.Provider, body.ProviderID.String())
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	if a.runJobs != nil {
		go func() { _, _ = a.runJobs(context.Background()) }()
	}
	writeJSON(w, http.StatusCreated, toTitleDTO(title))
}

// tagOptions lists the genre/tag vocabulary for filter pickers: one provider's with ?provider=, else all merged.
func (a *apiV1) tagOptions(w http.ResponseWriter, r *http.Request) {
	options, err := a.svc.ContentTagOptions(r.Context())
	if p := r.URL.Query().Get("provider"); p != "" {
		options, err = a.svc.ProviderTags(r.Context(), p)
	}
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	type tagDTO struct {
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		Group string `json:"group"`
	}
	options = visibleTagOptions(r.Context(), options)
	items := make([]tagDTO, 0, len(options))
	for _, o := range options {
		group := "Genres"
		if o.Kind != "genre" {
			group = tagCategoryLabel(o.Category)
		}
		items = append(items, tagDTO{Name: o.Name, Kind: o.Kind, Group: group})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *apiV1) wantedList(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.ListWanted(r.Context())
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": allowedManga(r.Context(), items)})
}

func (a *apiV1) wantedAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AniListID  json.Number `json:"anilist_id"`
		Provider   string      `json:"provider"`
		ProviderID json.Number `json:"provider_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	provider, id := body.Provider, body.ProviderID.String()
	if id == "" {
		provider, id = catalog.AniListProvider, body.AniListID.String()
	}
	item, err := a.svc.AddWanted(r.Context(), provider, id, func(m catalog.Manga) bool {
		return contentAllowed(r.Context(), m.IsAdult, mangaContentTags(m))
	})
	if errors.Is(err, service.ErrContentBlocked) {
		v1err(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		v1err(w, http.StatusBadGateway, "unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (a *apiV1) guardedManga(w http.ResponseWriter, r *http.Request, catalogID int64) bool {
	if m, err := a.svc.GetManga(r.Context(), catalogID); err != nil || !contentAllowed(r.Context(), m.IsAdult, mangaContentTags(m)) {
		v1err(w, http.StatusNotFound, "not_found", "not found")
		return false
	}
	return true
}

func (a *apiV1) matchesList(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Query(r, "catalog_id")
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !a.guardedManga(w, r, id) {
		return
	}
	matches, err := a.svc.ListMatches(r.Context(), id)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": matches})
}

func (a *apiV1) matchesFind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CatalogID int64 `json:"catalog_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !a.guardedManga(w, r, body.CatalogID) {
		return
	}
	matches, err := a.svc.MatchSources(r.Context(), body.CatalogID)
	if err != nil {
		v1err(w, http.StatusBadGateway, "unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": matches})
}

func (a *apiV1) track(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MatchID         int64  `json:"match_id"`
		Output          string `json:"output"`
		Monitored       *bool  `json:"monitored"`
		RefreshInterval string `json:"refresh_interval"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	match, err := a.svc.GetMatch(r.Context(), body.MatchID)
	if err != nil || !a.guardedManga(w, r, match.CatalogMangaID) {
		if err != nil {
			v1err(w, http.StatusNotFound, "not_found", "match not found")
		}
		return
	}
	monitored := body.Monitored == nil || *body.Monitored
	t, err := a.svc.TrackMatch(r.Context(), body.MatchID, body.Output, monitored, body.RefreshInterval)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toTitleDTO(t))
}

func (a *apiV1) jobsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := jobs.Filter{Type: q.Get("type"), Limit: clampLimit(q.Get("limit"))}
	if status := q.Get("status"); status != "" {
		f.Statuses = []string{status}
	}
	f.BeforeID, _ = strconv.ParseInt(q.Get("cursor"), 10, 64)
	list, err := a.svc.FindJobs(r.Context(), f)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	items := make([]jobDTO, 0, len(list))
	for _, j := range list {
		items = append(items, toJobDTO(j))
	}
	next := ""
	if len(list) == f.Limit {
		next = strconv.FormatInt(list[len(list)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (a *apiV1) jobGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	j, err := a.svc.GetJob(r.Context(), id)
	if err != nil {
		v1err(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	writeJSON(w, http.StatusOK, toJobDTO(j))
}

// jobCancel stops a running job (and its children) or marks a pending one
// cancelled — the API twin of the web UI's /ui/jobs/{id}/cancel. Gated by
// jobs.manage in requiredPerm.
func (a *apiV1) jobCancel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	if err := a.svc.CancelJob(r.Context(), id); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// titleScopedJobTypes may be enqueued with library.manage alone.
var titleScopedJobTypes = map[string]bool{
	jobs.TypeRefreshTitle:    true,
	jobs.TypeDownloadMissing: true,
	jobs.TypeScanDownloads:   true,
	jobs.TypeDetectCrops:     true,
}

// mayEnqueue enforces the split: jobs.manage for anything; library.manage for
// title-scoped jobs on any visible title — the same rule as the web UI's
// /ui/library/{id}/refresh|download|scan actions.
func (a *apiV1) mayEnqueue(r *http.Request, typ string, titleID int64) bool {
	user := userFrom(r.Context())
	if user.Can(auth.PermJobsManage) {
		return true
	}
	return user.Can(auth.PermLibraryManage) && titleScopedJobTypes[typ] && titleID > 0 &&
		titleAllowed(r.Context(), a.svc, titleID)
}

func (a *apiV1) jobEnqueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type      string `json:"type"`
		TitleID   int64  `json:"title_id"`
		SourceID  string `json:"source_id"`
		CatalogID int64  `json:"catalog_id"`
		Delay     string `json:"delay"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !a.mayEnqueue(r, body.Type, body.TitleID) {
		v1err(w, http.StatusForbidden, "forbidden", "missing permission for this job")
		return
	}
	runAfter := time.Now()
	if body.Delay != "" {
		d, err := time.ParseDuration(body.Delay)
		if err != nil || d < 0 || d > 24*time.Hour {
			v1err(w, http.StatusBadRequest, "bad_request", "invalid delay")
			return
		}
		runAfter = runAfter.Add(d)
	}
	var j jobs.Job
	var err error
	switch body.Type {
	case jobs.TypeVerifySource:
		j, err = a.svc.EnqueueSource(r.Context(), body.SourceID, runAfter)
	case jobs.TypeMatchSources:
		j, err = a.svc.EnqueueCatalog(r.Context(), body.Type, body.CatalogID, runAfter)
	default:
		j, err = a.svc.Enqueue(r.Context(), body.Type, body.TitleID, runAfter)
	}
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toJobDTO(j))
}

func jobsRunV1(runJobs func(context.Context) (service.RunSummary, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sum, err := runJobs(r.Context())
		if err != nil {
			v1err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"done": sum.Done, "failed": sum.Failed})
	}
}

// sourcesPick lists the minimal source picker for users without sources.manage.
func (a *apiV1) sourcesPick(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.SyncSources(r.Context(), ""); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	list, err := a.svc.ListSources(r.Context())
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	list = filterNSFWSources(r.Context(), list)
	items := make([]sourcePickDTO, 0, len(list))
	for _, s := range list {
		items = append(items, toSourcePickDTO(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
