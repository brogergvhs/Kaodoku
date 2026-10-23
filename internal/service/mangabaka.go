package service

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/mangabaka"
	"github.com/brogergvhs/kaodoku/internal/util"
)

const (
	SettingMangaBakaEnabled    = "mangabaka.enabled"
	SettingMangaBakaEvery      = "mangabaka.refresh_every"
	SettingMangaBakaVariant    = "mangabaka.variant"
	SettingMangaBakaAggressive = "mangabaka.aggressive"
)

var mangaBakaClient, _ = util.NewHTTPClient(util.HTTPClientOptions{UserAgent: "Kaodoku (+https://github.com/brogergvhs/kaodoku)"})

func (s *JobService) boolSetting(ctx context.Context, key string) bool {
	on, _ := strconv.ParseBool(s.Setting(ctx, key, SettingDefault(key)))
	return on
}

func (s *JobService) MangaBakaEnabled(ctx context.Context) bool {
	return s.boolSetting(ctx, SettingMangaBakaEnabled)
}

// mbOn: MangaBaka is turned on and its copy is loaded.
func (s *JobService) mbOn(ctx context.Context) bool { return s.MangaBakaEnabled(ctx) && s.mb.Ready() }

// mbAggressive: shared data comes only from MangaBaka and what trackers already sent.
func (s *JobService) mbAggressive(ctx context.Context) bool {
	return s.mbOn(ctx) && s.boolSetting(ctx, SettingMangaBakaAggressive)
}

// MangaBakaEvery is how often to check for a new dump; 0 while MangaBaka is off.
func (s *JobService) MangaBakaEvery(ctx context.Context) time.Duration {
	if !s.MangaBakaEnabled(ctx) {
		return 0
	}
	d, _ := time.ParseDuration(s.Setting(ctx, SettingMangaBakaEvery, SettingDefault(SettingMangaBakaEvery)))
	return d
}

func (s *JobService) MangaBakaReady() bool { return s.mb.Ready() }

func (s *JobService) mangaBakaVariant(ctx context.Context) string {
	return s.Setting(ctx, SettingMangaBakaVariant, SettingDefault(SettingMangaBakaVariant))
}

// MangaBakaUnchanged reports whether the published dump is the one already imported.
func (s *JobService) MangaBakaUnchanged(ctx context.Context) (bool, error) {
	v := s.mangaBakaVariant(ctx)
	sum, err := mangabaka.RemoteSHA1(ctx, mangaBakaClient, mangabaka.DumpURL(v))
	m := s.mb.Meta(ctx)
	return err == nil && sum == m.SHA1 && v == m.Variant, err
}

func (s *JobService) RefreshMangaBaka(ctx context.Context, force bool) error {
	_, err := s.enqueueExact(ctx, jobs.TypeMangaBakaRefresh, JobPayload{Force: force}, time.Now())
	return err
}

func (s *JobService) runMangaBakaRefresh(ctx context.Context, force bool, progress ProgressManager) error {
	if !s.MangaBakaEnabled(ctx) {
		return nil
	}
	handle := progress.Register("mangabaka")
	defer handle.MarkDone()
	v := s.mangaBakaVariant(ctx)
	if _, err := s.mb.Refresh(ctx, mangaBakaClient, mangabaka.DumpURL(v), v, force, func(n int) { handle.Update(n, 0, 0) }); err != nil {
		return err
	}
	_ = s.catalog.saveCalls(ctx, catalog.MangaBakaProvider, catalog.Call{Kind: "Database", Reason: "Database refresh"}, 1)
	return s.linkLibrary(ctx)
}

type MangaBakaStatus struct {
	mangabaka.Meta
	Next      time.Time
	LastError string
}

func (s *JobService) MangaBakaStatus(ctx context.Context) MangaBakaStatus {
	st := MangaBakaStatus{Meta: s.mb.Meta(ctx)}
	if every := s.MangaBakaEvery(ctx); every > 0 {
		if last := s.LastJobTime(ctx, jobs.TypeMangaBakaRefresh); !last.IsZero() {
			st.Next = last.Add(every)
		}
	}
	var status string
	_ = s.db.QueryRowContext(ctx, `SELECT status, last_error FROM jobs WHERE type = ? ORDER BY id DESC LIMIT 1`, jobs.TypeMangaBakaRefresh).Scan(&status, &st.LastError)
	if status != "failed" && status != "dead" {
		st.LastError = ""
	}
	return st
}

// mbSeries finds a work's MangaBaka series by its own id, else by its confirmed links when they all agree.
func mbSeries(m catalog.Manga, maps map[string]catalog.ExternalID, found map[string]mangabaka.Series) (mangabaka.Series, bool) {
	if sr, ok := found[m.Provider+":"+m.ProviderID]; ok {
		return sr, true
	}
	var sr mangabaka.Series
	for prov, e := range maps {
		hit, ok := found[prov+":"+e.ProviderID]
		if !e.Confirmed || !ok {
			continue
		}
		if sr.ID != 0 && sr.ID != hit.ID {
			return mangabaka.Series{}, false
		}
		sr = hit
	}
	return sr, sr.ID != 0
}

// mbLink links works to every tracker MangaBaka has them on, one-to-one only; ids that clash with the user's or an
// existing confirmed link become suggestions instead.
func (s *JobService) mbLink(ctx context.Context, works []catalog.Manga) {
	if !s.mbOn(ctx) {
		return
	}
	all := make([]map[string]catalog.ExternalID, len(works))
	refs := map[string][]string{}
	for i, m := range works {
		all[i] = s.mappings(ctx, m)
		for prov, e := range all[i] {
			if e.Confirmed && e.ProviderID != "" {
				refs[prov] = append(refs[prov], e.ProviderID)
			}
		}
	}
	found := s.mb.ByRefs(ctx, refs)
	for i, m := range works {
		sr, ok := mbSeries(m, all[i], found)
		if !ok {
			continue
		}
		for _, prov := range mangabaka.Trackers {
			id, e := sr.IDs[prov], all[i][prov]
			switch {
			case prov == m.Provider:
			case id == "" || e.ProviderID == id:
				s.setSuggestion(ctx, m.ID, prov, "")
			case e.Method == "user" || e.Confirmed && e.Method != catalog.MangaBakaProvider:
				s.setSuggestion(ctx, m.ID, prov, id)
			case !s.takenByOther(ctx, m.ID, prov, id):
				_ = s.catalog.repo.SetExternalID(ctx, m.ID, catalog.ExternalID{Provider: prov, ProviderID: id, Method: catalog.MangaBakaProvider, Confirmed: true})
			}
		}
	}
}

// setSuggestion keeps a dismissed suggestion dismissed while its id stays the same; an empty id drops it.
func (s *JobService) setSuggestion(ctx context.Context, catalogID int64, provider, id string) {
	if id == "" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM mangabaka_suggestions WHERE catalog_manga_id = ? AND provider = ?`, catalogID, provider)
		return
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO mangabaka_suggestions (catalog_manga_id, provider, provider_id) VALUES (?, ?, ?)
		ON CONFLICT (catalog_manga_id, provider) DO UPDATE SET dismissed = dismissed AND provider_id = excluded.provider_id, provider_id = excluded.provider_id`,
		catalogID, provider, id)
}

// linkLibrary runs MangaBaka linking over every library work.
func (s *JobService) linkLibrary(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT catalog_manga_id FROM titles WHERE catalog_manga_id IS NOT NULL`)
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
	var works []catalog.Manga
	for _, id := range ids {
		if m, err := s.catalog.GetManga(ctx, id); err == nil {
			works = append(works, m)
		}
	}
	for chunk := range slices.Chunk(works, 200) {
		s.mbLink(ctx, chunk)
	}
	return ctx.Err()
}

type MangaBakaLink struct {
	library.Title
	ID, URL string // the link MangaBaka made, or the id it suggests
	Current string // the existing link a suggestion clashes with; "" when marked as not on the tracker
}

// MangaBakaLinks lists a tracker's links made from MangaBaka and its open suggestions, over library titles.
func (s *JobService) MangaBakaLinks(ctx context.Context, provider string) (links, suggestions []MangaBakaLink, err error) {
	scan := func(q string) ([]MangaBakaLink, error) {
		rows, err := s.db.QueryContext(ctx, q, provider)
		if err != nil {
			return nil, err
		}
		byTitle := map[int64]MangaBakaLink{}
		var ids []int64
		for rows.Next() {
			var l MangaBakaLink
			if rows.Scan(&l.Title.ID, &l.ID, &l.Current) == nil {
				l.URL = catalog.MangaURL(provider, l.ID)
				byTitle[l.Title.ID], ids = l, append(ids, l.Title.ID)
			}
		}
		rows.Close()
		titles, err := s.lib.repo.TitlesByIDs(ctx, ids)
		out := make([]MangaBakaLink, 0, len(titles))
		for _, t := range titles {
			l := byTitle[t.ID]
			l.Title = t
			out = append(out, l)
		}
		slices.SortFunc(out, func(a, b MangaBakaLink) int {
			return strings.Compare(strings.ToLower(a.DisplayTitle), strings.ToLower(b.DisplayTitle))
		})
		return out, err
	}
	if links, err = scan(`SELECT t.id, e.provider_id, '' FROM titles t JOIN catalog_external_ids e ON e.catalog_manga_id = t.catalog_manga_id
		WHERE e.provider = ? AND e.method = 'mangabaka'`); err != nil {
		return nil, nil, err
	}
	suggestions, err = scan(`SELECT t.id, g.provider_id, COALESCE(e.provider_id, '') FROM titles t
		JOIN mangabaka_suggestions g ON g.catalog_manga_id = t.catalog_manga_id AND g.dismissed = 0
		LEFT JOIN catalog_external_ids e ON e.catalog_manga_id = g.catalog_manga_id AND e.provider = g.provider
		WHERE g.provider = ?`)
	return links, suggestions, err
}

// ConfirmMangaBakaLink makes a MangaBaka link the user's own.
func (s *JobService) ConfirmMangaBakaLink(ctx context.Context, titleID int64, provider string) error {
	m, ok := s.titleWork(ctx, titleID)
	if e := s.mappings(ctx, m)[provider]; ok && e.Method == catalog.MangaBakaProvider {
		return s.LinkExternalID(ctx, titleID, provider, e.ProviderID)
	}
	return nil
}

// AnswerMangaBakaSuggestion links the suggested id, or dismisses it until MangaBaka suggests another.
func (s *JobService) AnswerMangaBakaSuggestion(ctx context.Context, titleID int64, provider string, use bool) error {
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return fmt.Errorf("title has no catalog entry")
	}
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT provider_id FROM mangabaka_suggestions WHERE catalog_manga_id = ? AND provider = ?`, m.ID, provider).Scan(&id); err != nil {
		return err
	}
	if !use {
		_, err := s.db.ExecContext(ctx, `UPDATE mangabaka_suggestions SET dismissed = 1 WHERE catalog_manga_id = ? AND provider = ?`, m.ID, provider)
		return err
	}
	if err := s.LinkExternalID(ctx, titleID, provider, id); err != nil {
		return err
	}
	s.setSuggestion(ctx, m.ID, provider, "")
	return nil
}

// mbUnify adds every id MangaBaka knows for each work, so listings of one work from different trackers merge.
func (s *JobService) mbUnify(ctx context.Context, items []catalog.Manga) []catalog.Manga {
	if len(items) == 0 || !s.mbOn(ctx) {
		return items
	}
	refs := map[string][]string{}
	for _, m := range items {
		refs[m.Provider] = append(refs[m.Provider], m.ProviderID)
	}
	found := s.mb.ByRefs(ctx, refs)
	for i, m := range items {
		sr, ok := found[m.Provider+":"+m.ProviderID]
		if !ok {
			continue
		}
		ids := maps.Clone(m.ExternalIDs)
		if ids == nil {
			ids = map[string]string{}
		}
		for p, id := range sr.IDs {
			ids[p] = cmp.Or(ids[p], id)
		}
		ids[catalog.MangaBakaProvider] = strconv.FormatInt(sr.ID, 10)
		items[i].ExternalIDs = ids
	}
	return items
}

// mbManga shows a series under the first tracker the user can use that knows it.
func (s *JobService) mbManga(ctx context.Context, sr mangabaka.Series) (catalog.Manga, bool) {
	for _, p := range s.Providers(ctx) {
		if id := sr.IDs[p.Name()]; id != "" {
			return catalog.FromMangaBaka(sr, p.Name(), id), true
		}
	}
	return catalog.Manga{}, false
}

func (s *JobService) mbMangas(ctx context.Context, list []mangabaka.Series) []catalog.Manga {
	var out []catalog.Manga
	for _, sr := range list {
		if m, ok := s.mbManga(ctx, sr); ok {
			out = append(out, m)
		}
	}
	return out
}

func (s *JobService) mbRelated(ctx context.Context, seriesID string, limit int) (relations, recommendations []catalog.Manga) {
	id, _ := strconv.ParseInt(seriesID, 10, 64)
	rels, recs := s.mb.Related(ctx, id, limit)
	return s.mbMangas(ctx, rels), s.mbMangas(ctx, recs)
}

func (s *JobService) mbTrending(ctx context.Context, limit int) []catalog.Manga {
	if !s.mbOn(ctx) {
		return nil
	}
	return s.mbMangas(ctx, s.mb.Trending(ctx, limit))
}

// mbRefresh updates a work's release status and final counts from MangaBaka, filling only blank details, when its
// data is under 8 days old or aggressive mode is on.
func (s *JobService) mbRefresh(ctx context.Context, m *catalog.Manga) bool {
	if !s.mbOn(ctx) {
		return false
	}
	sr, ok := s.mb.ByProvider(ctx, m.Provider, m.ProviderID)
	if !ok || time.Since(sr.UpdatedAt) >= 8*24*time.Hour && !s.mbAggressive(ctx) {
		return false
	}
	fresh := catalog.FromMangaBaka(sr, m.Provider, m.ProviderID)
	m.Status = cmp.Or(fresh.Status, m.Status)
	if sr.Status == "completed" || sr.Status == "cancelled" {
		m.Chapters, m.Volumes = cmp.Or(fresh.Chapters, m.Chapters), cmp.Or(fresh.Volumes, m.Volumes)
	}
	fresh.Chapters = nil
	fillBlanks(m, fresh)
	return true
}

// mbFinalChapters is a finished series' chapter count on MangaBaka, or 0.
func (s *JobService) mbFinalChapters(ctx context.Context, malID string) int {
	if !s.mbOn(ctx) {
		return 0
	}
	if sr, ok := s.mb.ByProvider(ctx, catalog.MALProvider, malID); ok && (sr.Status == "completed" || sr.Status == "cancelled") {
		return sr.Chapters
	}
	return 0
}
