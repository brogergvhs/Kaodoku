package server

import (
	"context"
	"fmt"
	"net/http"
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
	Parsers   int
	Version   string
	Error     string
}

type kotatsuRowView struct {
	Source   kotatsu.Source
	Imported bool
}

type kotatsuBrowseView struct {
	Rows   []kotatsuRowView
	Total  int
	Capped bool
	Error  string
}

func (u *webUI) kotatsuStatus(ctx context.Context) kotatsuStatusView {
	kc := u.svc.KotatsuClient(ctx)
	if kc == nil {
		return kotatsuStatusView{}
	}
	v := kotatsuStatusView{Enabled: true}
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	h, err := kc.Health(hctx)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Reachable, v.Version = h.OK, h.Version
	sctx, scancel := context.WithTimeout(ctx, 3*time.Second)
	defer scancel()
	if srcs, err := kc.Sources(sctx); err == nil {
		for _, s := range srcs {
			if kotatsuVisible(ctx, s) {
				v.Parsers++
			}
		}
	}
	return v
}

func (u *webUI) kotatsuBrowse(w http.ResponseWriter, r *http.Request) {
	kc := u.svc.KotatsuClient(r.Context())
	if kc == nil {
		u.fail(w, fmt.Errorf("kotatsu worker is not enabled"))
		return
	}
	sctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	srcs, err := kc.Sources(sctx)
	if err != nil {
		u.frag(w, "kotatsuBrowse", kotatsuBrowseView{Error: err.Error()})
		return
	}
	imported := map[string]bool{}
	if existing, err := u.svc.ListSources(r.Context()); err == nil {
		for _, s := range existing {
			imported[s.ID] = true
		}
	}
	q := strings.ToLower(strings.TrimSpace(r.FormValue("q")))
	rows := make([]kotatsuRowView, 0, 64)
	for _, s := range srcs {
		if !kotatsuVisible(r.Context(), s) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(s.Name), q) &&
			!strings.Contains(strings.ToLower(s.ID), q) &&
			!strings.Contains(strings.ToLower(strings.Join(s.Domains, " ")), q) {
			continue
		}
		rows = append(rows, kotatsuRowView{Source: s, Imported: imported[strings.ToLower(kotatsu.ScraperPrefix+s.ID)]})
	}
	v := kotatsuBrowseView{Total: len(rows)}
	if len(rows) > kotatsuBrowseCap {
		rows = rows[:kotatsuBrowseCap]
		v.Capped = true
	}
	v.Rows = rows
	u.frag(w, "kotatsuBrowse", v)
}

func (u *webUI) kotatsuImport(w http.ResponseWriter, r *http.Request) {
	kc := u.svc.KotatsuClient(r.Context())
	if kc == nil {
		u.fail(w, fmt.Errorf("kotatsu worker is not enabled"))
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	sctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	srcs, err := kc.Sources(sctx)
	if err != nil {
		u.fail(w, err)
		return
	}
	for _, s := range srcs {
		if s.ID != id {
			continue
		}
		if s.Broken {
			u.fail(w, fmt.Errorf("source %s is marked broken upstream", s.Name))
			return
		}
		if s.NSFW && !adultAllowed(r.Context()) {
			u.fail(w, fmt.Errorf("adult sources are not allowed for this user"))
			return
		}
		version := ""
		hctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		if h, err := kc.Health(hctx); err == nil {
			version = h.Version
		}
		cancel()
		if err := u.svc.ImportKotatsuSource(r.Context(), s, version); err != nil {
			u.fail(w, err)
			return
		}
		u.frag(w, "kotatsuRow", kotatsuRowView{Source: s, Imported: true})
		if all, err := u.svc.ListSources(r.Context()); err == nil {
			_, kot := splitKotatsuSources(u.sourceRows(r.Context(), filterNSFWSources(r.Context(), all)))
			imported := strings.ToLower(kotatsu.ScraperPrefix + s.ID)
			for i := range kot {
				kot[i].Poll = kot[i].Source.ID == imported
			}
			u.frag(w, "kotatsuSourcesTableOOB", kot)
		}
		return
	}
	u.fail(w, fmt.Errorf("worker has no source %q", id))
}
