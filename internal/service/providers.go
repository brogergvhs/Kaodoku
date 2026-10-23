package service

import (
	"cmp"
	"context"
	"encoding/json"
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
	mappingRecheck = 14 * 24 * time.Hour
	autoConfirm    = 0.92
	suggestMatch   = 0.75
	relatedTTL     = 7 * 24 * time.Hour
	tagsTTL        = 30 * 24 * time.Hour

	settingTagsRefreshed = "catalog.tags_refreshed_at"
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

// ProviderOutage is why a provider is unusable right now: its last reported outage, or a tag list still to sync.
func (s *JobService) ProviderOutage(name string) string {
	if o := s.healthOutage(name); o != "" {
		return o
	}
	if s.vocabPending(name) {
		return "syncing its tags and genres"
	}
	return ""
}

func (s *JobService) healthOutage(name string) string {
	if h, ok := s.catalog.providers[name].(interface{ Outage() string }); ok {
		return h.Outage()
	}
	return ""
}

// WatchProviders saves request counts every minute, syncs missing tag lists and re-checks trackers in an outage; working trackers are never pinged, failing requests report outages.
func (s *JobService) WatchProviders(ctx context.Context) {
	retries := map[string]outageRetry{}
	for {
		s.catalog.FlushRequests(ctx)
		s.SyncPendingVocabularies(ctx)
		s.probeOutages(ctx, retries, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

type outageRetry struct {
	wait time.Duration
	at   time.Time
}

// probeOutages re-checks each tracker in an outage 1, 2, 4… up to 30 minutes apart until it answers again.
func (s *JobService) probeOutages(ctx context.Context, retries map[string]outageRetry, now time.Time) {
	for _, name := range providerOrder {
		r, seen := retries[name]
		switch {
		case !s.providerConfigured(ctx, name) || s.healthOutage(name) == "":
			delete(retries, name)
		case !seen:
			retries[name] = outageRetry{time.Minute, now.Add(time.Minute)}
		case !now.Before(r.at):
			pctx, cancel := context.WithTimeout(catalog.WithReason(ctx, "Outage check"), 30*time.Second)
			_, _ = s.catalog.providers[name].Trending(pctx, 1)
			cancel()
			r.wait = min(2*r.wait, 30*time.Minute)
			retries[name] = outageRetry{r.wait, now.Add(r.wait)}
		}
	}
}

type callCounter interface {
	Count(catalog.Call, int64)
	TakeCalls() map[catalog.Call]int64
}

const imageCall = "Image"

// CountImage counts an image the server fetches itself from a tracker's servers.
func (s *JobService) CountImage(url, reason string) {
	if c, ok := s.catalog.providers[catalog.ImageProvider(url)].(callCounter); ok {
		c.Count(catalog.Call{Kind: imageCall, Reason: reason}, 1)
	}
}

type RequestCounts struct{ Today, Month, Total int64 }

func (c RequestCounts) plus(o RequestCounts) RequestCounts {
	return RequestCounts{c.Today + o.Today, c.Month + o.Month, c.Total + o.Total}
}

type TrackerRequests struct {
	Label       string
	API, Images RequestCounts
	All         bool
}

type TrackerCall struct {
	Reason, Kind string
	RequestCounts
}

type TrackerCalls struct {
	Label string
	Calls []TrackerCall
	Total RequestCounts
}

type trackerCallRow struct {
	provider string
	TrackerCall
}

// FlushRequests saves the tracker calls counted since the last flush.
func (s *JobService) FlushRequests(ctx context.Context) { s.catalog.FlushRequests(ctx) }

func (s *JobService) trackerCallRows(ctx context.Context) []trackerCallRow {
	s.catalog.FlushRequests(ctx)
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT provider, reason, kind,
		COALESCE(SUM(CASE WHEN day = ? THEN count END), 0), COALESCE(SUM(CASE WHEN day > ? THEN count END), 0), SUM(count)
		FROM tracker_calls GROUP BY provider, reason, kind ORDER BY SUM(count) DESC, reason, kind`, now.Format(time.DateOnly), now.AddDate(0, 0, -30).Format(time.DateOnly))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []trackerCallRow
	for rows.Next() {
		var r trackerCallRow
		if rows.Scan(&r.provider, &r.Reason, &r.Kind, &r.Today, &r.Month, &r.Total) == nil {
			out = append(out, r)
		}
	}
	return out
}

// TrackerRequestStats totals API calls and image fetches per tracker, with a final all-trackers row.
func (s *JobService) TrackerRequestStats(ctx context.Context) []TrackerRequests {
	byName := map[string]TrackerRequests{}
	for _, r := range s.trackerCallRows(ctx) {
		t := byName[r.provider]
		if r.Kind == imageCall {
			t.Images = t.Images.plus(r.RequestCounts)
		} else {
			t.API = t.API.plus(r.RequestCounts)
		}
		byName[r.provider] = t
	}
	var out []TrackerRequests
	all := TrackerRequests{Label: "All trackers", All: true}
	for _, name := range providerOrder {
		if r, ok := byName[name]; ok {
			r.Label = s.ProviderLabel(name)
			out = append(out, r)
			all.API, all.Images = all.API.plus(r.API), all.Images.plus(r.Images)
		}
	}
	if len(out) > 1 {
		out = append(out, all)
	}
	return out
}

// TrackerCallDetails breaks each tracker's calls down by reason and kind, busiest first, after the same breakdown across all trackers.
func (s *JobService) TrackerCallDetails(ctx context.Context) []TrackerCalls {
	byName := map[string]*TrackerCalls{}
	all := &TrackerCalls{Label: "All trackers"}
	merged := map[[2]string]int{}
	for _, r := range s.trackerCallRows(ctx) {
		t := byName[r.provider]
		if t == nil {
			t = &TrackerCalls{Label: s.ProviderLabel(r.provider)}
			byName[r.provider] = t
		}
		t.Calls, t.Total = append(t.Calls, r.TrackerCall), t.Total.plus(r.RequestCounts)
		all.Total = all.Total.plus(r.RequestCounts)
		if i, ok := merged[[2]string{r.Reason, r.Kind}]; ok {
			all.Calls[i].RequestCounts = all.Calls[i].plus(r.RequestCounts)
		} else {
			merged[[2]string{r.Reason, r.Kind}] = len(all.Calls)
			all.Calls = append(all.Calls, r.TrackerCall)
		}
	}
	slices.SortStableFunc(all.Calls, func(a, b TrackerCall) int { return cmp.Compare(b.Total, a.Total) })
	var out []TrackerCalls
	for _, name := range providerOrder {
		if t := byName[name]; t != nil {
			out = append(out, *t)
		}
	}
	if len(out) > 1 {
		out = append([]TrackerCalls{*all}, out...)
	}
	return out
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
	ctx = catalog.WithReason(ctx, "Search")
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return nil, false, err
	}
	if tags, _ := s.catalog.repo.ProviderTags(ctx, p.Name()); len(tags) > 0 {
		keys := map[string]string{}
		for _, t := range tags {
			keys[strings.ToLower(t.Name)] = cmp.Or(t.Slug, t.Name)
		}
		known := func(names []string) []string {
			var out []string
			for _, n := range names {
				if k, ok := keys[strings.ToLower(strings.TrimSpace(n))]; ok {
					out = append(out, k)
				}
			}
			return out
		}
		filter.GenreIn, filter.TagIn, filter.GenreNotIn, filter.TagNotIn = known(filter.GenreIn), known(filter.TagIn), known(filter.GenreNotIn), known(filter.TagNotIn)
	}
	return s.catalog.Search(ctx, p, query, limit, filter)
}

func (s *JobService) AddWanted(ctx context.Context, provider, id string, allowed func(catalog.Manga) bool) (catalog.Manga, error) {
	ctx = catalog.WithReason(ctx, "Wanted")
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return catalog.Manga{}, err
	}
	return s.catalog.AddWanted(ctx, p, id, allowed)
}

func (s *JobService) AddCatalogTitle(ctx context.Context, provider, id string) (library.Title, error) {
	ctx = catalog.WithReason(ctx, "Title added")
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
	ctx = catalog.WithReason(ctx, "Title details")
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
	ctx = catalog.WithReason(ctx, "Trending")
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

// resolveID returns only confirmed ids: stored mapping, MangaBaka, cross-links, then a scored title search.
// Aggressive MangaBaka mode stops after MangaBaka.
func (s *JobService) resolveID(ctx context.Context, m catalog.Manga, p catalog.Provider) string {
	name := p.Name()
	maps := s.mappings(ctx, m)
	if e, ok := maps[name]; ok && (e.Confirmed || e.Method == "user") {
		return e.ProviderID
	}
	if s.mbOn(ctx) {
		s.mbLink(ctx, []catalog.Manga{m})
		if id := s.confirmedID(ctx, m, name); id != "" || !s.mbAggressive(ctx) {
			return id
		}
		if _, ok := maps[name]; !ok {
			_ = s.catalog.repo.SetExternalID(ctx, m.ID, catalog.ExternalID{Provider: name, Method: "miss"})
		}
		return ""
	}
	if e, ok := maps[name]; ok {
		if t, err := database.ParseTime(e.CheckedAt); err == nil && time.Since(t) < mappingRecheck {
			return ""
		}
	}
	if src, ok := s.catalog.providers[m.Provider]; ok && crossLinks(m.Provider) && time.Since(m.UpdatedAt) >= mappingRecheck && s.providerEnabled(ctx, m.Provider) {
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
	var best catalog.Manga
	score := 0.0
	for _, q := range matchQueries(m) {
		cands, _, err := p.Search(ctx, q, 5, catalog.SearchFilter{})
		if err != nil {
			return ""
		}
		for _, c := range cands {
			if sc := catalog.MatchScore(m, c); sc > score {
				best, score = c, sc
			}
		}
		if score >= autoConfirm {
			break
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

// recommends is false for Kitsu, whose API has no community recommendations.
func recommends(provider string) bool { return provider != catalog.KitsuProvider }

// matchQueries lists the distinct romaji, English and native titles, since trackers index different ones.
func matchQueries(m catalog.Manga) []string {
	var out []string
	for _, t := range []string{m.TitleRomaji, m.TitleEnglish, m.TitleNative} {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
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

// relatedCached keeps a work's relations and recommendations in the database for a week; a stale copy stands in while a refresh fails.
func (s *JobService) relatedCached(ctx context.Context, p catalog.Provider, id string) ([]catalog.Manga, []catalog.Manga, error) {
	var relJSON, recJSON, fetched string
	var rel, recs []catalog.Manga
	err := s.db.QueryRowContext(ctx, `SELECT relations_json, recommendations_json, fetched_at FROM tracker_related WHERE provider = ? AND provider_id = ?`, p.Name(), id).Scan(&relJSON, &recJSON, &fetched)
	stored := err == nil && json.Unmarshal([]byte(relJSON), &rel) == nil && json.Unmarshal([]byte(recJSON), &recs) == nil
	if t, err := database.ParseTime(fetched); stored && err == nil && time.Since(t) < relatedTTL {
		return rel, recs, nil
	}
	freshRel, freshRecs, err := p.Related(ctx, id, 24)
	if err != nil && !catalog.IsNotFound(err) {
		if stored {
			return rel, recs, nil
		}
		return nil, nil, err
	}
	relData, _ := json.Marshal(freshRel)
	recData, _ := json.Marshal(freshRecs)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO tracker_related (provider, provider_id, relations_json, recommendations_json, fetched_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (provider, provider_id) DO UPDATE SET relations_json = excluded.relations_json, recommendations_json = excluded.recommendations_json, fetched_at = excluded.fetched_at`,
		p.Name(), id, relData, recData, database.FormatTime(time.Now()))
	s.trimRelated(ctx)
	return freshRel, freshRecs, nil
}

func (s *JobService) RelatedManga(ctx context.Context, catalogID int64, limit int) (relations, recommendations []catalog.Manga, err error) {
	ctx = catalog.WithReason(ctx, "Related titles")
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
	for _, k := range catalog.Keys(s.mbUnify(ctx, []catalog.Manga{m})[0]) {
		self[k] = true
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
		rel, rec, rerr := s.relatedCached(ctx, p, e.ProviderID)
		if rerr != nil {
			err = rerr
			continue
		}
		if !connected || pref.Relations {
			rels = append(rels, s.mbUnify(ctx, s.fillFromStore(ctx, p.Name(), rel)))
		}
		if !connected || pref.Recommendations {
			recs = append(recs, s.mbUnify(ctx, s.fillFromStore(ctx, p.Name(), rec)))
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
				hit.m.Sources = nil
				out = append(out, hit)
			} else {
				fillBlanks(&hit.m, m)
			}
			if !slices.Contains(hit.m.Sources, m.Provider) {
				hit.m.Sources = append(hit.m.Sources, m.Provider)
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

// fillBlanks copies what a record lacks from another listing of the same work.
func fillBlanks(dst *catalog.Manga, src catalog.Manga) {
	dst.Description = cmp.Or(dst.Description, src.Description)
	dst.CoverImage = cmp.Or(dst.CoverImage, src.CoverImage)
	dst.TitleEnglish = cmp.Or(dst.TitleEnglish, src.TitleEnglish)
	dst.TitleNative = cmp.Or(dst.TitleNative, src.TitleNative)
	dst.RelationType = cmp.Or(dst.RelationType, src.RelationType)
	if len(dst.Genres) == 0 {
		dst.Genres = src.Genres
	}
	if len(dst.Authors) == 0 {
		dst.Authors = src.Authors
	}
	if dst.Chapters == nil {
		dst.Chapters = src.Chapters
	}
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
	ctx = catalog.WithReason(ctx, "Recommendations")
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
	for _, entries := range s.trackerLists(ctx, uid, s.connectedTrackers(ctx, uid)) {
		listed := make([]catalog.Manga, len(entries))
		for i, e := range entries {
			listed[i] = e.Manga
		}
		for _, m := range s.mbUnify(ctx, listed) {
			for _, k := range catalog.Keys(m) {
				exclude[k] = true
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

// runCatalogRefresh skips finished works and refreshes tag lists monthly.
func (s *JobService) runCatalogRefresh(ctx context.Context, progress ProgressManager) error {
	ctx = catalog.WithReason(ctx, "Catalog refresh")
	var errs []error
	if t, err := database.ParseTime(s.Setting(ctx, settingTagsRefreshed, "")); err != nil || time.Since(t) >= tagsTTL {
		if err := s.refreshTagVocabulary(ctx); err != nil {
			errs = append(errs, err)
		} else if err := s.SetSetting(ctx, settingTagsRefreshed, database.FormatTime(time.Now())); err != nil {
			errs = append(errs, err)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT cm.id FROM titles t JOIN catalog_manga cm ON cm.id = t.catalog_manga_id WHERE cm.status NOT IN ('FINISHED', 'CANCELLED')`)
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

type ProviderLink struct {
	Provider, Label, ID, Method, URL, Cover, Outage string
	Confirmed, Primary, Connected, Syncing          bool
}

func (s *JobService) TitleLinks(ctx context.Context, titleID int64) ([]ProviderLink, error) {
	ctx = catalog.WithReason(ctx, "Tracker panel")
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return nil, fmt.Errorf("title has no catalog entry")
	}
	maps := s.mappings(ctx, m)
	var connected []string
	for _, a := range s.TrackerAccounts(ctx, auth.UserID(ctx)) {
		connected = append(connected, a.Provider)
	}
	syncing := s.progressiveTrackers(ctx, auth.UserID(ctx), titleID)
	link := func(name, label string) ProviderLink {
		e := maps[name]
		return ProviderLink{Provider: name, Label: label, ID: e.ProviderID, Method: e.Method, Confirmed: e.Confirmed, Primary: name == m.Provider, Connected: slices.Contains(connected, name), Syncing: slices.Contains(syncing, name), URL: catalog.MangaURL(name, e.ProviderID), Outage: s.ProviderOutage(name)}
	}
	primary := link(m.Provider, s.ProviderLabel(m.Provider))
	primary.Cover = m.CoverImage
	out := []ProviderLink{primary}
	for _, p := range s.Trackers(ctx) {
		if p.Name() == m.Provider {
			continue
		}
		l := link(p.Name(), p.Label())
		if l.ID != "" {
			l.Cover = s.linkCover(ctx, p.Name(), l.ID)
		}
		out = append(out, l)
	}
	return out, nil
}

// linkCover prefers a stored record, then MangaBaka, before asking the tracker.
func (s *JobService) linkCover(ctx context.Context, provider, id string) string {
	if m, ok, _ := s.catalog.repo.MangaByProvider(ctx, provider, id); ok {
		return m.CoverImage
	}
	if s.mbOn(ctx) {
		if sr, ok := s.mb.ByProvider(ctx, provider, id); ok {
			return sr.CoverURL(250)
		}
	}
	if c, err := s.CatalogManga(ctx, provider, id); err == nil {
		return c.CoverImage
	}
	return ""
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
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tracker_ratings WHERE title_id = ? AND provider = ?`, titleID, provider); err != nil {
		return err
	}
	return s.catalog.repo.SetExternalID(ctx, m.ID, catalog.ExternalID{Provider: provider, ProviderID: id, Method: "user", Confirmed: id != ""})
}

func (s *JobService) SwitchTitleMetadata(ctx context.Context, titleID int64, provider, id string) error {
	ctx = catalog.WithReason(ctx, "Metadata switch")
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
