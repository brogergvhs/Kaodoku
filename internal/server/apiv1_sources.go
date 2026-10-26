package server

import (
	"context"
	"net/http"
	"time"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/service"
)

// v1 source linking + management: the web title Sources section and the
// /sources page on the wire, backed by the same service calls.

type linkedSourceDTO struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Active   bool   `json:"active"`
}

type titleSourcesDTO struct {
	Linked  []linkedSourceDTO `json:"linked"`
	Matches []catalog.Match   `json:"matches"`
	Finding bool              `json:"finding"`
	Failed  bool              `json:"failed"`
	Error   string            `json:"error,omitempty"`
	Sources []sourcePickDTO   `json:"sources"`
}

// titleSources mirrors the web title page's Sources section: linked sources,
// usable match candidates (unless a find job is still running), and the
// enabled sources available for manual URL linking.
func (a *apiV1) titleSources(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	title, err := a.svc.GetTitle(ctx, id)
	if err != nil {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	all, _ := a.svc.ListSources(ctx)
	all = filterNSFWSources(ctx, all)
	names, usable := map[string]string{}, map[string]bool{}
	for _, s := range all {
		names[s.ID] = s.Name
		usable[s.ID] = s.Enabled
	}
	links, _ := a.svc.ListTitleSources(ctx, id)
	out := titleSourcesDTO{Linked: []linkedSourceDTO{}, Matches: []catalog.Match{}, Sources: []sourcePickDTO{}}
	linked := map[string]bool{}
	for _, l := range links {
		linked[l.SourceID] = true
		name := names[l.SourceID]
		if name == "" {
			name = l.SourceID
		}
		out.Linked = append(out.Linked, linkedSourceDTO{
			SourceID: l.SourceID, Name: name, URL: l.URL, Active: l.URL == title.SourceURL,
		})
	}
	if title.CatalogMangaID != nil {
		cid := *title.CatalogMangaID
		out.Finding, out.Failed, out.Error = jobStateForSvc(ctx, a.svc, jobs.TypeMatchSources, service.JobPayload{CatalogID: cid})
		if !out.Finding {
			matches, _ := a.svc.ListMatches(ctx, cid)
			for _, m := range matches {
				if !linked[m.SourceID] && usable[m.SourceID] {
					out.Matches = append(out.Matches, m)
				}
			}
		}
	}
	for _, s := range all {
		if s.Enabled && !linked[s.ID] {
			out.Sources = append(out.Sources, toSourcePickDTO(s))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// sourcesFind re-runs source matching for a title, exactly like the web:
// cached decisions are cleared so the re-search doesn't echo stale results.
func (a *apiV1) sourcesFind(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	title, err := a.svc.GetTitle(r.Context(), id)
	if err != nil {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	if title.CatalogMangaID == nil {
		v1err(w, http.StatusBadRequest, "bad_request", "no catalog metadata to search from")
		return
	}
	if err := a.svc.ClearMatches(r.Context(), *title.CatalogMangaID); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if _, err := a.svc.EnqueueCatalog(r.Context(), jobs.TypeMatchSources, *title.CatalogMangaID, time.Now()); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	a.kick()
	w.WriteHeader(http.StatusAccepted)
}

// sourcesLink attaches a match candidate to the title.
func (a *apiV1) sourcesLink(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	var body struct {
		MatchID int64 `json:"match_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if _, err := a.svc.LinkTitleSource(r.Context(), id, body.MatchID); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a.kick()
	w.WriteHeader(http.StatusNoContent)
}

// sourcesLinkURL links a specific source page URL, the web's manual path.
func (a *apiV1) sourcesLinkURL(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	var body struct {
		SourceID string `json:"source_id"`
		URL      string `json:"url"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if _, err := a.svc.LinkTitleSourceURL(r.Context(), id, body.SourceID, body.URL); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a.kick()
	w.WriteHeader(http.StatusNoContent)
}

// sourcesUnlink removes a linked source URL from the title.
func (a *apiV1) sourcesUnlink(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.svc.UnlinkTitleSource(r.Context(), id, body.URL); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sourceManageDTO is the /sources page row (gated sources.manage).
type sourceManageDTO struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	NSFW          bool   `json:"nsfw"`
	Origin        string `json:"origin"`
	Status        string `json:"status"`
	LastCheckedAt string `json:"last_checked_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	ChaptersFound int    `json:"chapters_found"`
}

// sourcesManage lists all sources with health, mirroring the web /sources page.
func (a *apiV1) sourcesManage(w http.ResponseWriter, r *http.Request) {
	srcs, err := a.svc.ListSources(r.Context())
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	srcs = filterNSFWSources(r.Context(), srcs)
	items := make([]sourceManageDTO, 0, len(srcs))
	for _, s := range srcs {
		items = append(items, sourceManageDTO{
			ID: s.ID, Name: s.Name, Enabled: s.Enabled, NSFW: s.NSFW, Origin: s.Origin,
			Status: s.Status, LastCheckedAt: s.LastCheckedAt, LastError: s.LastError,
			ChaptersFound: s.ChaptersFound,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// sourceSetEnabled toggles a source, like the web row switch.
func (a *apiV1) sourceSetEnabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.svc.SetSourceEnabled(r.Context(), r.PathValue("id"), body.On); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sourceVerify queues a health re-verification for one source.
func (a *apiV1) sourceVerify(w http.ResponseWriter, r *http.Request) {
	if _, err := a.svc.EnqueueSource(r.Context(), r.PathValue("id"), time.Now()); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	a.kick()
	w.WriteHeader(http.StatusAccepted)
}

// titleActivity reports the title's running/queued jobs and last failure,
// mirroring the web's activity banner (shared titleActivityFrom logic).
func (a *apiV1) titleActivity(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedTitleID(w, r)
	if !ok {
		return
	}
	title, err := a.svc.GetTitle(r.Context(), id)
	if err != nil {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	js, _ := a.svc.FindJobs(r.Context(), jobs.Filter{TitleIDs: []int64{title.ID}, Newest: true})
	running, label, queued, failed, msg := titleActivityFrom(js, title)
	if queued == nil {
		queued = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active": label, "queued": queued, "failed": failed, "error": msg,
		"running": running,
	})
}

// kick runs the job loop in the background so queued work starts immediately.
func (a *apiV1) kick() {
	if a.runJobs == nil {
		return
	}
	go func() { _, _ = a.runJobs(context.Background()) }()
}
