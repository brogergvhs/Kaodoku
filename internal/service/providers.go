package service

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
)

const (
	SettingMALClientID       = "mal.client_id"
	SettingMALClientSecret   = "mal.client_secret"
	SettingProvidersDefault  = "providers.default"
	SettingProvidersDisabled = "providers.disabled"
)

const (
	mappingRecheck  = 14 * 24 * time.Hour
	autoConfirm     = 0.92
	suggestMatch    = 0.75
	relatedCacheTTL = 30 * time.Minute
)

func ProviderNames() []string { return slices.Clone(providerOrder) }

func (s *JobService) ProviderLabel(name string) string {
	if p, ok := s.catalog.providers[name]; ok {
		return p.Label()
	}
	return name
}

func validProviderList(value string) error {
	for _, name := range splitNames(value) {
		if !slices.Contains(providerOrder, name) {
			return fmt.Errorf("unknown provider %q (use %s)", name, strings.Join(providerOrder, ", "))
		}
	}
	return nil
}

func splitNames(value string) []string {
	var out []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *JobService) providerEnabled(ctx context.Context, name string) bool {
	return s.providerConfigured(ctx, name) && s.ProviderOutage(name) == ""
}

// ProviderOutage is the last outage a provider reported, empty while it is reachable.
func (s *JobService) ProviderOutage(name string) string {
	if h, ok := s.catalog.providers[name].(interface{ Outage() string }); ok {
		return h.Outage()
	}
	return ""
}

// WatchProviders pings providers in outage every minute and the rest every ten, so outages switch them off server-wide.
func (s *JobService) WatchProviders(ctx context.Context) {
	for tick := 0; ; tick++ {
		for _, name := range providerOrder {
			if s.providerConfigured(ctx, name) && (tick%10 == 0 || s.ProviderOutage(name) != "") {
				pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, _ = s.catalog.providers[name].Trending(pctx, 1)
				cancel()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func (s *JobService) providerConfigured(ctx context.Context, name string) bool {
	p, ok := s.catalog.providers[name]
	if !ok || slices.Contains(splitNames(s.Setting(ctx, SettingProvidersDisabled, "")), name) {
		return false
	}
	if m, ok := p.(interface{ SetClientID(string) }); ok {
		id := s.Setting(ctx, SettingMALClientID, "")
		m.SetClientID(id)
		return id != ""
	}
	return true
}

func (s *JobService) Providers(ctx context.Context) []catalog.Provider {
	return s.providers(ctx, s.providerEnabled)
}

// Trackers also lists providers that are in an outage.
func (s *JobService) Trackers(ctx context.Context) []catalog.Provider {
	return s.providers(ctx, s.providerConfigured)
}

func (s *JobService) providers(ctx context.Context, ok func(context.Context, string) bool) []catalog.Provider {
	user := auth.FromContext(ctx)
	var out []catalog.Provider
	for _, name := range providerOrder {
		if ok(ctx, name) && user.ProviderAllowed(name) {
			out = append(out, s.catalog.providers[name])
		}
	}
	return out
}

func (s *JobService) Provider(ctx context.Context, name string) (catalog.Provider, error) {
	if name == "" {
		name = s.DefaultProvider(ctx)
	}
	if s.providerEnabled(ctx, name) && auth.FromContext(ctx).ProviderAllowed(name) {
		return s.catalog.providers[name], nil
	}
	if outage := s.ProviderOutage(name); outage != "" {
		return nil, fmt.Errorf("%s is temporarily unavailable (%s)", s.ProviderLabel(name), outage)
	}
	return nil, fmt.Errorf("metadata provider %q is not available", name)
}

// DefaultProvider: earliest-connected available tracker, else the server default, else the first available.
func (s *JobService) DefaultProvider(ctx context.Context) string {
	available := s.Providers(ctx)
	has := func(name string) bool {
		return slices.ContainsFunc(available, func(p catalog.Provider) bool { return p.Name() == name })
	}
	if len(available) == 0 {
		return catalog.AniListProvider
	}
	for _, a := range s.TrackerAccounts(ctx, auth.UserID(ctx)) {
		if has(a.Provider) {
			return a.Provider
		}
	}
	if d := s.Setting(ctx, SettingProvidersDefault, catalog.AniListProvider); has(d) {
		return d
	}
	return available[0].Name()
}

func (s *JobService) SearchManga(ctx context.Context, provider, query string, limit int, filter catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return nil, false, err
	}
	return s.catalog.Search(ctx, p, query, limit, filter)
}

func (s *JobService) AddWanted(ctx context.Context, provider, id string, allowed func(catalog.Manga) bool) (catalog.Manga, error) {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return catalog.Manga{}, err
	}
	return s.catalog.AddWanted(ctx, p, id, allowed)
}

func (s *JobService) AddCatalogTitle(ctx context.Context, provider, id string) (library.Title, error) {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return library.Title{}, err
	}
	if inLib, _ := s.lib.TitlesByProvider(ctx, p.Name()); inLib[id] > 0 {
		return s.lib.GetTitle(ctx, inLib[id])
	}
	title, err := s.catalog.AddCatalogTitle(ctx, p, id)
	if err != nil {
		return library.Title{}, err
	}
	s.titleAdded(ctx, auth.UserID(ctx), title.ID)
	if err := s.enqueueSourceSearchForTitle(ctx, title); err != nil {
		return title, err
	}
	return title, nil
}

func (s *JobService) CatalogManga(ctx context.Context, provider, id string) (catalog.Manga, error) {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return catalog.Manga{}, err
	}
	if m, ok, err := s.catalog.repo.MangaByProvider(ctx, p.Name(), id); err != nil || ok {
		return m, err
	}
	return s.catalog.fetch(ctx, p, id)
}

func (s *JobService) LibraryIndex(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	for _, name := range providerOrder {
		ids, _ := s.lib.TitlesByProvider(ctx, name)
		for id, tid := range ids {
			out[name+":"+id] = tid
		}
	}
	return out
}

func (s *JobService) TrendingManga(ctx context.Context, limit int) ([]catalog.Manga, error) {
	p, err := s.Provider(ctx, "")
	if err != nil {
		return nil, err
	}
	return p.Trending(ctx, limit)
}

func (s *JobService) mappings(ctx context.Context, m catalog.Manga) map[string]catalog.ExternalID {
	out := map[string]catalog.ExternalID{}
	ids, _ := s.catalog.repo.ExternalIDs(ctx, m.ID)
	for _, e := range ids {
		out[e.Provider] = e
	}
	out[m.Provider] = catalog.ExternalID{Provider: m.Provider, ProviderID: m.ProviderID, Method: "native", Confirmed: true}
	return out
}

func (s *JobService) confirmedID(ctx context.Context, m catalog.Manga, provider string) string {
	if e := s.mappings(ctx, m)[provider]; e.Confirmed {
		return e.ProviderID
	}
	return ""
}

// resolveID returns only confirmed ids: stored mapping, cross-links, then a scored title search.
func (s *JobService) resolveID(ctx context.Context, m catalog.Manga, p catalog.Provider) string {
	name := p.Name()
	maps := s.mappings(ctx, m)
	if e, ok := maps[name]; ok && (e.Confirmed || e.Method == "user") {
		return e.ProviderID
	}
	if e, ok := maps[name]; ok {
		if t, err := database.ParseTime(e.CheckedAt); err == nil && time.Since(t) < mappingRecheck {
			return ""
		}
	}
	if src, ok := s.catalog.providers[m.Provider]; ok && crossLinks(m.Provider) && s.providerEnabled(ctx, m.Provider) {
		if fresh, err := s.catalog.fetch(ctx, src, m.ProviderID); err == nil {
			if id := s.confirmedID(ctx, fresh, name); id != "" {
				return id
			}
		}
	}
	if r, ok := p.(catalog.Resolver); ok {
		for prov, e := range maps {
			if prov == name || !e.Confirmed || e.ProviderID == "" {
				continue
			}
			if id, found, err := r.Resolve(ctx, prov, e.ProviderID); err == nil && found {
				_ = s.catalog.repo.SetExternalID(ctx, m.ID, catalog.ExternalID{Provider: name, ProviderID: id, Method: "cross_link", Confirmed: true})
				return id
			}
		}
	}
	cands, _, err := p.Search(ctx, firstTitle(m), 5, catalog.SearchFilter{})
	if err != nil {
		return ""
	}
	var best catalog.Manga
	score := 0.0
	for _, c := range cands {
		if sc := catalog.MatchScore(m, c); sc > score {
			best, score = c, sc
		}
	}
	e := catalog.ExternalID{Provider: name, Method: "miss"}
	if score >= suggestMatch {
		e = catalog.ExternalID{Provider: name, ProviderID: best.ProviderID, Method: "title_match", Confirmed: score >= autoConfirm && !s.takenByOther(ctx, m.ID, name, best.ProviderID)}
	}
	_ = s.catalog.repo.SetExternalID(ctx, m.ID, e)
	if e.Confirmed {
		return e.ProviderID
	}
	return ""
}

func crossLinks(provider string) bool {
	return provider == catalog.AniListProvider || provider == catalog.KitsuProvider
}

func firstTitle(m catalog.Manga) string {
	for _, t := range catalog.Titles(m) {
		if strings.TrimSpace(t) != "" {
			return t
		}
	}
	return ""
}

func (s *JobService) takenByOther(ctx context.Context, catalogID int64, provider, id string) bool {
	inLib, _ := s.lib.TitlesByProvider(ctx, provider)
	tid := inLib[id]
	if tid == 0 {
		return false
	}
	t, err := s.lib.GetTitle(ctx, tid)
	return err == nil && (t.CatalogMangaID == nil || *t.CatalogMangaID != catalogID)
}

func (s *JobService) resolveLibrary(ctx context.Context, p catalog.Provider, limit int) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT cm.id FROM titles t JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE cm.provider != ? AND NOT EXISTS (
			SELECT 1 FROM catalog_external_ids e WHERE e.catalog_manga_id = cm.id AND e.provider = ?
			AND (e.confirmed = 1 OR e.method = 'user' OR e.checked_at > ?))
		LIMIT ?`, p.Name(), p.Name(), database.FormatTime(time.Now().Add(-mappingRecheck)), limit)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if m, err := s.catalog.GetManga(ctx, id); err == nil && ctx.Err() == nil {
			s.resolveID(ctx, m, p)
		}
	}
}

type cachedRelated struct {
	relations, recs []catalog.Manga
	expires         time.Time
}

func (s *JobService) relatedCached(ctx context.Context, p catalog.Provider, id string, limit int) ([]catalog.Manga, []catalog.Manga, error) {
	key := fmt.Sprintf("%s:%s:%d", p.Name(), id, limit)
	s.relMu.Lock()
	c, ok := s.relCache[key]
	s.relMu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.relations, c.recs, nil
	}
	rel, recs, err := p.Related(ctx, id, limit)
	if err != nil {
		return nil, nil, err
	}
	s.relMu.Lock()
	if s.relCache == nil {
		s.relCache = map[string]cachedRelated{}
	}
	s.relCache[key] = cachedRelated{rel, recs, time.Now().Add(relatedCacheTTL)}
	s.relMu.Unlock()
	return rel, recs, nil
}

func (s *JobService) RelatedManga(ctx context.Context, catalogID int64, limit int) (relations, recommendations []catalog.Manga, err error) {
	m, err := s.catalog.GetManga(ctx, catalogID)
	if err != nil {
		return nil, nil, err
	}
	maps := s.mappings(ctx, m)
	self := map[string]bool{}
	for prov, e := range maps {
		if e.Confirmed && e.ProviderID != "" {
			self[prov+":"+e.ProviderID] = true
		}
	}
	prefs := map[string]TrackerPrefs{}
	for _, a := range s.TrackerAccounts(ctx, auth.UserID(ctx)) {
		prefs[a.Provider] = a.TrackerPrefs
	}
	var rels, recs [][]catalog.Manga
	for _, p := range s.Providers(ctx) {
		e := maps[p.Name()]
		pref, connected := prefs[p.Name()]
		if !e.Confirmed || e.ProviderID == "" || connected && !pref.Relations && !pref.Recommendations {
			continue
		}
		rel, rec, rerr := s.relatedCached(ctx, p, e.ProviderID, limit)
		if rerr != nil {
			err = rerr
			continue
		}
		if !connected || pref.Relations {
			rels = append(rels, rel)
		}
		if !connected || pref.Recommendations {
			recs = append(recs, rec)
		}
	}
	relations, recommendations = mergeManga(rels, self, 0), mergeManga(recs, self, limit)
	if len(relations)+len(recommendations) > 0 {
		err = nil
	}
	return relations, recommendations, err
}

// mergeManga dedupes works across providers by any shared id, most-agreed first.
func mergeManga(lists [][]catalog.Manga, exclude map[string]bool, limit int) []catalog.Manga {
	type ranked struct {
		m     catalog.Manga
		votes int
		order int
	}
	var out []*ranked
	byKey := map[string]*ranked{}
	for _, list := range lists {
		for _, m := range list {
			keys := catalog.Keys(m)
			if slices.ContainsFunc(keys, func(k string) bool { return exclude[k] }) {
				continue
			}
			var hit *ranked
			for _, k := range keys {
				if r := byKey[k]; r != nil {
					hit = r
				}
			}
			if hit == nil {
				hit = &ranked{m: m, order: len(out)}
				out = append(out, hit)
			}
			hit.votes++
			for _, k := range keys {
				byKey[k] = hit
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].votes != out[j].votes {
			return out[i].votes > out[j].votes
		}
		return out[i].order < out[j].order
	})
	items := make([]catalog.Manga, 0, len(out))
	for _, r := range out {
		if limit > 0 && len(items) >= limit {
			break
		}
		items = append(items, r.m)
	}
	return items
}

func (s *JobService) recentWorks(ctx context.Context, userID int64, limit int) []int64 {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.catalog_manga_id FROM chapter_read_progress rp
		JOIN chapters c ON c.id = rp.chapter_id
		JOIN titles t ON t.id = c.title_id
		WHERE rp.user_id = ? AND t.catalog_manga_id IS NOT NULL
		GROUP BY t.catalog_manga_id ORDER BY MAX(rp.last_read_at) DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func (s *JobService) recommendedManga(ctx context.Context, limit int) ([]catalog.Manga, bool, error) {
	uid := auth.UserID(ctx)
	seeds := s.recentWorks(ctx, uid, 24)
	const maxSeeds = 4
	if len(seeds) > maxSeeds {
		spread := make([]int64, 0, maxSeeds)
		for i := 0; i < maxSeeds; i++ {
			spread = append(spread, seeds[i*len(seeds)/maxSeeds])
		}
		seeds = spread
	}
	exclude := map[string]bool{}
	for k := range s.LibraryIndex(ctx) {
		exclude[k] = true
	}
	for _, name := range s.connectedTrackers(ctx, uid) {
		if p, actx, ok := s.trackerSession(ctx, uid, name); ok {
			entries, _ := p.ListEntries(actx)
			for _, e := range entries {
				for _, k := range catalog.Keys(e.Manga) {
					exclude[k] = true
				}
			}
		}
	}
	var lists [][]catalog.Manga
	for _, id := range seeds {
		rel, recs, err := s.RelatedManga(ctx, id, limit)
		if err != nil {
			continue
		}
		var list []catalog.Manga
		for _, m := range append(rel, recs...) {
			if m.Format == "MANGA" || m.Format == "" {
				list = append(list, m)
			}
		}
		lists = append(lists, list)
	}
	out := mergeManga(lists, exclude, 0)
	if len(out) == 0 {
		items, err := s.TrendingManga(ctx, limit)
		return items, false, err
	}
	return out, true, nil
}

func (s *JobService) runCatalogRefresh(ctx context.Context, progress ProgressManager) error {
	var errs []error
	if err := s.refreshTagVocabulary(ctx); err != nil {
		errs = append(errs, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT cm.id FROM titles t JOIN catalog_manga cm ON cm.id = t.catalog_manga_id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	handle := progress.Register("catalog refresh")
	handle.SetTotal(len(ids))
	defer handle.MarkDone()
	for i, id := range ids {
		handle.Update(i+1, len(ids), 0)
		m, err := s.catalog.GetManga(ctx, id)
		if err != nil {
			continue
		}
		if err := s.refreshWork(ctx, m); err != nil && !catalog.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("refresh %s %s: %w", m.Provider, m.ProviderID, err))
		}
	}
	return errs2err(errs)
}

func (s *JobService) refreshWork(ctx context.Context, m catalog.Manga) error {
	p, ok := s.catalog.providers[m.Provider]
	if !ok || !s.providerEnabled(ctx, m.Provider) {
		return nil
	}
	fresh, rels, err := p.Get(ctx, m.ProviderID)
	if err != nil {
		return err
	}
	for prov, e := range s.mappings(ctx, m) {
		if fresh.Description != "" && len(fresh.Authors) > 0 && fresh.CoverImage != "" {
			break
		}
		if prov == m.Provider || !e.Confirmed || e.ProviderID == "" || !s.providerEnabled(ctx, prov) {
			continue
		}
		if sec, _, err := s.catalog.providers[prov].Get(ctx, e.ProviderID); err == nil {
			fillBlanks(&fresh, sec)
		}
	}
	if _, err := s.catalog.repo.UpsertManga(ctx, fresh); err != nil {
		return err
	}
	return s.catalog.repo.ReplaceRelations(ctx, catalog.RelKey(m.Provider, m.ProviderID), relKeys(m.Provider, rels))
}

func fillBlanks(dst *catalog.Manga, src catalog.Manga) {
	if dst.Description == "" {
		dst.Description = src.Description
	}
	if len(dst.Authors) == 0 {
		dst.Authors = src.Authors
	}
	if dst.CoverImage == "" {
		dst.CoverImage = src.CoverImage
	}
}

type ProviderLink struct {
	Provider, Label, ID, Method, URL, Cover string
	Confirmed, Primary, Connected, Syncing  bool
}

func (s *JobService) TitleLinks(ctx context.Context, titleID int64) ([]ProviderLink, error) {
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return nil, fmt.Errorf("title has no catalog entry")
	}
	maps := s.mappings(ctx, m)
	connected := s.connectedTrackers(ctx, auth.UserID(ctx))
	syncing := s.progressiveTrackers(ctx, auth.UserID(ctx), titleID)
	link := func(name, label string) ProviderLink {
		e := maps[name]
		return ProviderLink{Provider: name, Label: label, ID: e.ProviderID, Method: e.Method, Confirmed: e.Confirmed, Primary: name == m.Provider, Connected: slices.Contains(connected, name), Syncing: slices.Contains(syncing, name), URL: catalog.MangaURL(name, e.ProviderID)}
	}
	primary := link(m.Provider, s.ProviderLabel(m.Provider))
	primary.Cover = m.CoverImage
	out := []ProviderLink{primary}
	for _, p := range s.Providers(ctx) {
		if p.Name() == m.Provider {
			continue
		}
		l := link(p.Name(), p.Label())
		if l.ID != "" {
			if c, err := s.CatalogManga(ctx, p.Name(), l.ID); err == nil {
				l.Cover = c.CoverImage
			}
		}
		out = append(out, l)
	}
	return out, nil
}

func (s *JobService) MetadataProvider(ctx context.Context, titleID int64) string {
	m, _ := s.titleWork(ctx, titleID)
	return m.Provider
}

func (s *JobService) TitleLinkedID(ctx context.Context, titleID int64, provider string) string {
	if m, ok := s.titleWork(ctx, titleID); ok {
		return s.confirmedID(ctx, m, provider)
	}
	return ""
}

func (s *JobService) LinkExternalID(ctx context.Context, titleID int64, provider, id string) error {
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return fmt.Errorf("title has no catalog entry")
	}
	if _, err := s.Provider(ctx, provider); err != nil {
		return err
	}
	if id != "" && s.takenByOther(ctx, m.ID, provider, id) {
		return fmt.Errorf("that %s entry is already linked to another library title", s.ProviderLabel(provider))
	}
	if id == "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM tracker_title_sync WHERE title_id = ? AND provider = ?`, titleID, provider); err != nil {
			return err
		}
	}
	return s.catalog.repo.SetExternalID(ctx, m.ID, catalog.ExternalID{Provider: provider, ProviderID: id, Method: "user", Confirmed: id != ""})
}

func (s *JobService) SwitchTitleMetadata(ctx context.Context, titleID int64, provider, id string) error {
	old, ok := s.titleWork(ctx, titleID)
	if !ok {
		return fmt.Errorf("title has no catalog entry")
	}
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return err
	}
	if p.Name() == old.Provider && id == old.ProviderID {
		return nil
	}
	if s.takenByOther(ctx, old.ID, p.Name(), id) {
		return fmt.Errorf("that %s entry is already linked to another library title", p.Label())
	}
	next, err := s.catalog.fetch(ctx, p, id)
	if err != nil {
		return err
	}
	if err := s.catalog.repo.CopyExternalIDs(ctx, old.ID, next.ID); err != nil {
		return err
	}
	if err := s.catalog.repo.SetExternalID(ctx, next.ID, catalog.ExternalID{Provider: old.Provider, ProviderID: old.ProviderID, Method: "user", Confirmed: true}); err != nil {
		return err
	}
	if err := s.catalog.repo.MoveMatches(ctx, old.ID, next.ID); err != nil {
		return err
	}
	return s.lib.repo.SetTitleCatalog(ctx, titleID, next.ID)
}

func (s *JobService) titleWork(ctx context.Context, titleID int64) (catalog.Manga, bool) {
	t, err := s.lib.GetTitle(ctx, titleID)
	if err != nil || t.CatalogMangaID == nil {
		return catalog.Manga{}, false
	}
	m, err := s.catalog.GetManga(ctx, *t.CatalogMangaID)
	return m, err == nil
}
