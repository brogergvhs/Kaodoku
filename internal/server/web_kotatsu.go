package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
)

const kotatsuBrowseCap = 200

func adultAllowed(ctx context.Context) bool {
	u := auth.FromContext(ctx)
	return u != nil && u.AllowAdult
}

func kotatsuVisible(ctx context.Context, s kotatsu.Source) bool {
	return !s.Broken && (!s.NSFW || adultAllowed(ctx))
}

type kotatsuStatusView struct {
	Enabled   bool
	Reachable bool
	Error     string
}

type kotatsuRowView struct {
	Source   kotatsu.Source
	Imported bool
}

type kotatsuBrowseView struct {
	Rows      []kotatsuRowView
	Total     int
	Available int
	Capped    bool
	Filtered  bool
	Error     string
	Filter    kotatsuFilter
	Languages []string
	Types     []string
}

type kotatsuFilter struct {
	Q, Language, Type, Status string
}

func (f kotatsuFilter) any() bool {
	return f.Q != "" || f.Language != "" || f.Type != "" || f.Status != ""
}

func (u *webUI) kotatsuStatus(ctx context.Context) kotatsuStatusView {
	kc := u.svc.KotatsuClient(ctx)
	if kc == nil {
		return kotatsuStatusView{}
	}
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	h, err := kc.Health(hctx)
	if err != nil {
		return kotatsuStatusView{Enabled: true, Error: err.Error()}
	}
	return kotatsuStatusView{Enabled: true, Reachable: h.OK}
}

func (u *webUI) kotatsuModal(w http.ResponseWriter, r *http.Request) {
	u.frag(w, "kotatsuModal", u.kotatsuBrowseView(r))
}

func (u *webUI) kotatsuBrowse(w http.ResponseWriter, r *http.Request) {
	u.frag(w, "kotatsuBrowse", u.kotatsuBrowseView(r))
}

func (u *webUI) kotatsuBrowseView(r *http.Request) kotatsuBrowseView {
	f := kotatsuFilter{
		Q:        strings.TrimSpace(r.FormValue("q")),
		Language: strings.TrimSpace(r.FormValue("lang")),
		Type:     strings.TrimSpace(r.FormValue("type")),
		Status:   strings.TrimSpace(r.FormValue("status")),
	}
	v := kotatsuBrowseView{Filter: f, Filtered: f.any()}
	kc := u.svc.KotatsuClient(r.Context())
	if kc == nil {
		v.Error = "kotatsu worker is not enabled"
		return v
	}
	sctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	srcs, err := kc.Sources(sctx)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	imported := map[string]bool{}
	if existing, err := u.svc.ListSources(r.Context()); err == nil {
		for _, s := range existing {
			imported[s.ID] = true
		}
	}
	langs, types := map[string]bool{}, map[string]bool{}
	q := strings.ToLower(f.Q)
	rows := make([]kotatsuRowView, 0, 64)
	for _, s := range srcs {
		if !kotatsuVisible(r.Context(), s) {
			continue
		}
		v.Available++
		if s.Language != "" {
			langs[s.Language] = true
		}
		if s.ContentType != "" {
			types[s.ContentType] = true
		}
		if f.Language != "" && s.Language != "" && s.Language != f.Language {
			continue
		}
		if f.Type != "" && s.ContentType != f.Type {
			continue
		}
		got := imported[strings.ToLower(kotatsu.ScraperPrefix+s.ID)]
		if (f.Status == "imported" && !got) || (f.Status == "new" && got) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(s.Name), q) &&
			!strings.Contains(strings.ToLower(s.ID), q) &&
			!strings.Contains(strings.ToLower(strings.Join(s.Domains, " ")), q) {
			continue
		}
		rows = append(rows, kotatsuRowView{Source: s, Imported: got})
	}
	v.Total = len(rows)
	if len(rows) > kotatsuBrowseCap {
		rows = rows[:kotatsuBrowseCap]
		v.Capped = true
	}
	v.Rows = rows
	v.Languages, v.Types = slices.Sorted(maps.Keys(langs)), slices.Sorted(maps.Keys(types))
	return v
}

func (u *webUI) kotatsuImport(w http.ResponseWriter, r *http.Request) {
	kc := u.svc.KotatsuClient(r.Context())
	if kc == nil {
		u.fail(w, fmt.Errorf("kotatsu worker is not enabled"))
		return
	}
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	ids := r.Form["id"]
	if len(ids) == 0 {
		u.fail(w, fmt.Errorf("select at least one source"))
		return
	}
	sctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	srcs, err := kc.Sources(sctx)
	if err != nil {
		u.fail(w, err)
		return
	}
	byID := make(map[string]kotatsu.Source, len(srcs))
	for _, s := range srcs {
		byID[s.ID] = s
	}
	picked := make([]kotatsu.Source, 0, len(ids))
	for _, id := range ids {
		s, ok := byID[strings.TrimSpace(id)]
		if !ok {
			u.fail(w, fmt.Errorf("worker has no source %q", id))
			return
		}
		if s.Broken {
			u.fail(w, fmt.Errorf("source %s is marked broken upstream", s.Name))
			return
		}
		if s.NSFW && !adultAllowed(r.Context()) {
			u.fail(w, fmt.Errorf("adult sources are not allowed for this user"))
			return
		}
		picked = append(picked, s)
	}
	version := ""
	hctx, hcancel := context.WithTimeout(r.Context(), 3*time.Second)
	if h, err := kc.Health(hctx); err == nil {
		version = h.Version
	}
	hcancel()
	for _, s := range picked {
		if err := u.svc.ImportKotatsuSource(r.Context(), s, version); err != nil {
			u.fail(w, err)
			return
		}
	}
	u.frag(w, "kotatsuBrowse", u.kotatsuBrowseView(r))
	if all, err := u.svc.ListSources(r.Context()); err == nil {
		_, kot := splitKotatsuSources(u.sourceRows(r.Context(), filterNSFWSources(r.Context(), all)))
		fresh := make(map[string]bool, len(picked))
		for _, s := range picked {
			fresh[strings.ToLower(kotatsu.ScraperPrefix+s.ID)] = true
		}
		for i := range kot {
			kot[i].Poll = fresh[kot[i].Source.ID]
		}
		u.frag(w, "kotatsuSourcesTableOOB", kot)
	}
}
