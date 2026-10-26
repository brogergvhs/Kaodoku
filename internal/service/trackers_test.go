package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
)

type fakeProvider struct {
	name    string
	works   map[string]catalog.Manga
	search  []catalog.Manga
	byQuery map[string][]catalog.Manga
	listErr error
	dates   bool
	mu      sync.Mutex
	entries map[string]catalog.Entry
	saves   []string
}

func newFake(name string) *fakeProvider {
	return &fakeProvider{name: name, works: map[string]catalog.Manga{}, entries: map[string]catalog.Entry{}}
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Label() string {
	return map[string]string{catalog.AniListProvider: "AniList", catalog.MALProvider: "MyAnimeList", catalog.KitsuProvider: "Kitsu", catalog.MangaUpdatesProvider: "MangaUpdates"}[f.name]
}
func (f *fakeProvider) Get(_ context.Context, id string) (catalog.Manga, []catalog.Relation, error) {
	m, ok := f.works[id]
	if !ok {
		return catalog.Manga{}, nil, catalog.ErrNotFound
	}
	return m, nil, nil
}
func (f *fakeProvider) Search(_ context.Context, q string, _ int, _ catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	if f.byQuery != nil {
		return f.byQuery[q], false, nil
	}
	return f.search, false, nil
}
func (f *fakeProvider) Trending(context.Context, int) ([]catalog.Manga, error) { return f.search, nil }
func (f *fakeProvider) Related(context.Context, string, int) ([]catalog.Manga, []catalog.Manga, error) {
	return nil, f.search, nil
}
func (f *fakeProvider) Viewer(context.Context) (string, string, error) { return "7", "me", nil }
func (f *fakeProvider) ListEntries(context.Context) ([]catalog.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []catalog.Entry
	for id, e := range f.entries {
		e.Manga = catalog.Manga{Provider: f.name, ProviderID: id}
		out = append(out, e)
	}
	return out, f.listErr
}
func (f *fakeProvider) GetEntry(_ context.Context, id string) (catalog.Entry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	return e, ok, f.listErr
}
func (f *fakeProvider) SaveEntry(_ context.Context, id string, u catalog.EntryUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	save := fmt.Sprintf("%s:%d:%s", id, u.Progress, u.Status)
	if f.dates {
		save += fmt.Sprintf(":%s:%s", day(u.Started), day(u.Finished))
	}
	if u.Score != 0 {
		save += fmt.Sprintf(":score=%d", u.Score)
	}
	f.saves = append(f.saves, save)
	e := f.entries[id]
	if u.Score != 0 {
		e.Score = max(u.Score, 0)
	}
	if u.Progress >= 0 {
		e.Progress = u.Progress
	}
	if u.Status != "" {
		e.Status = u.Status
	}
	if !u.Started.IsZero() {
		e.Started = u.Started
	}
	if !u.Finished.IsZero() {
		e.Finished = u.Finished
	}
	f.entries[id] = e
	return nil
}

func (f *fakeProvider) TracksDates() bool { return f.dates }

func day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.DateOnly)
}
func (f *fakeProvider) DeleteEntry(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, id)
	return nil
}
func (f *fakeProvider) savesCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.saves...)
}

func openTestJobs(t *testing.T) (*JobService, context.Context) {
	t.Helper()
	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeDB)
	tagsSynced(svc)
	return svc, ctx
}

func tagsSynced(svc *JobService) {
	for _, name := range providerOrder {
		svc.vocabReady.Store(name, true)
	}
}

func connect(t *testing.T, svc *JobService, ctx context.Context, provider string, created time.Time) {
	t.Helper()
	token, err := svc.secrets.Encrypt("tok-" + provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO tracker_accounts (user_id, provider, access_token, remote_user_id, remote_name, created_at) VALUES (1, ?, ?, '7', 'me', ?)`, provider, token, created.UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
}

// seedTitle adds a library title with chapters 1..n, the first read of them
// completed by user 1, who has progressive sync on for every tracker.
func seedTitle(t *testing.T, svc *JobService, ctx context.Context, m catalog.Manga, n, read int) (int64, catalog.Manga) {
	t.Helper()
	stored, err := svc.catalog.repo.UpsertManga(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.db.ExecContext(ctx, `INSERT INTO titles (catalog_manga_id, source_url, display_title) VALUES (?, ?, 't')`, stored.ID, "seed://"+m.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	titleID, _ := res.LastInsertId()
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO tracker_title_sync (user_id, title_id, provider) SELECT 1, ?, value FROM json_each(?)`, titleID, `["anilist","mal","kitsu","mangaupdates"]`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		res, err := svc.db.ExecContext(ctx, `INSERT INTO chapters (title_id, label, url, number_main) VALUES (?, ?, ?, ?)`, titleID, fmt.Sprint(i), fmt.Sprintf("seed://%d/%d", titleID, i), i)
		if err != nil {
			t.Fatal(err)
		}
		if i <= read {
			chID, _ := res.LastInsertId()
			if _, err := svc.db.ExecContext(ctx, `INSERT INTO chapter_read_progress (user_id, chapter_id, completed) VALUES (1, ?, 1)`, chID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return titleID, stored
}

func asUser(svc *JobService, ctx context.Context, t *testing.T) context.Context {
	u, err := svc.auth.GetUser(ctx, 1)
	if err != nil || u == nil {
		t.Fatalf("env admin missing: %v", err)
	}
	return auth.WithUser(ctx, u)
}

func TestUnauthorizedTrackerDisconnectsOnlyItself(t *testing.T) {
	svc, ctx := openTestJobs(t)
	anilist, kitsu := newFake(catalog.AniListProvider), newFake(catalog.KitsuProvider)
	anilist.listErr = catalog.ErrUnauthorized
	svc.catalog.providers[catalog.AniListProvider], svc.catalog.providers[catalog.KitsuProvider] = anilist, kitsu
	connect(t, svc, ctx, catalog.AniListProvider, time.Now())
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())

	err := svc.runTrackerSync(ctx, 1, catalog.AniListProvider, true, nil)
	if err == nil || !strings.Contains(err.Error(), "reconnect AniList") {
		t.Fatalf("sync error = %v", err)
	}
	accounts := svc.TrackerAccounts(ctx, 1)
	if len(accounts) != 1 || accounts[0].Provider != catalog.KitsuProvider {
		t.Fatalf("accounts after revoke = %+v, want only kitsu", accounts)
	}
}

func TestProgressFlowsFromTheSourceSide(t *testing.T) {
	svc, ctx := openTestJobs(t)
	anilist, mal := newFake(catalog.AniListProvider), newFake(catalog.MALProvider)
	svc.catalog.providers[catalog.AniListProvider], svc.catalog.providers[catalog.MALProvider] = anilist, mal
	if err := svc.SetSetting(ctx, SettingMALClientID, "client"); err != nil {
		t.Fatal(err)
	}
	connect(t, svc, ctx, catalog.AniListProvider, time.Now())
	connect(t, svc, ctx, catalog.MALProvider, time.Now())
	work := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", TitleRomaji: "Berserk", Status: "RELEASING", ExternalIDs: map[string]string{catalog.MALProvider: "10"}}
	anilist.works["1"] = work
	titleID, _ := seedTitle(t, svc, ctx, work, 8, 0)
	if err := svc.SetTrackerPrefs(asUser(svc, ctx, t), catalog.MALProvider, TrackerPrefs{Source: SourceTracker}); err != nil {
		t.Fatal(err)
	}
	mal.entries["10"] = catalog.Entry{Progress: 5, Status: "CURRENT"}
	sync := func(name string) {
		if err := svc.runTrackerSync(ctx, 1, name, false, nil); err != nil {
			t.Fatalf("%s sync: %v", name, err)
		}
	}
	sync(catalog.MALProvider)
	sync(catalog.AniListProvider)
	if got := anilist.savesCopy(); len(got) != 1 || got[0] != "1:5:CURRENT" {
		t.Fatalf("anilist saves = %v, want MAL's progress pushed up", got)
	}
	if got := mal.savesCopy(); len(got) != 0 {
		t.Fatalf("mal saves = %v, want nothing sent to the source", got)
	}
	anilist.entries["1"] = catalog.Entry{Progress: 7, Status: "CURRENT"}
	sync(catalog.AniListProvider)
	if progress, _, _ := svc.localTrackerState(ctx, 1, titleID); progress != 5 {
		t.Fatalf("local progress = %d, want AniList's 7 ignored while Kaodoku is its source", progress)
	}
}

func TestUnconfirmedMatchIsNeverPushed(t *testing.T) {
	svc, ctx := openTestJobs(t)
	mu := newFake(catalog.MangaUpdatesProvider)
	svc.catalog.providers[catalog.MangaUpdatesProvider] = mu
	svc.catalog.providers[catalog.AniListProvider] = newFake(catalog.AniListProvider)
	work := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", TitleRomaji: "Vagabond", Year: 1998, Format: "MANGA"}
	_, stored := seedTitle(t, svc, ctx, work, 3, 1)

	mu.search = []catalog.Manga{{Provider: catalog.MangaUpdatesProvider, ProviderID: "55", TitleRomaji: "Vagabond", Format: "MANGA"}}
	if id := svc.resolveID(ctx, stored, mu); id != "" {
		t.Fatalf("title-only match resolved to %q, want unconfirmed", id)
	}
	if e := svc.mappings(ctx, stored)[catalog.MangaUpdatesProvider]; e.ProviderID != "55" || e.Confirmed {
		t.Fatalf("mapping = %+v, want unconfirmed suggestion 55", e)
	}
	connect(t, svc, ctx, catalog.MangaUpdatesProvider, time.Now())
	if err := svc.runTrackerSync(ctx, 1, catalog.MangaUpdatesProvider, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := mu.savesCopy(); len(got) != 0 {
		t.Fatalf("pushed to an unconfirmed match: %v", got)
	}

	if _, err := svc.db.ExecContext(ctx, `DELETE FROM catalog_external_ids`); err != nil {
		t.Fatal(err)
	}
	mu.search[0].Year = 1998
	if id := svc.resolveID(ctx, stored, mu); id != "55" {
		t.Fatalf("exact title+year+format resolved to %q, want 55", id)
	}
}

func TestTitleMatchSearchesEnglishWhenRomajiMisses(t *testing.T) {
	svc, ctx := openTestJobs(t)
	mu := newFake(catalog.MangaUpdatesProvider)
	svc.catalog.providers[catalog.MangaUpdatesProvider] = mu
	svc.catalog.providers[catalog.AniListProvider] = newFake(catalog.AniListProvider)
	work := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "115240", TitleRomaji: "Kaljibui Ai", TitleEnglish: "Child of the Sheath", TitleNative: "칼집의 아이", Year: 2020, Format: "MANGA"}
	_, stored := seedTitle(t, svc, ctx, work, 1, 0)
	mu.byQuery = map[string][]catalog.Manga{
		"Kaljibui Ai":         {{Provider: catalog.MangaUpdatesProvider, ProviderID: "1", TitleRomaji: "Ai Ai House", Year: 2001, Format: "MANGA"}},
		"Child of the Sheath": {{Provider: catalog.MangaUpdatesProvider, ProviderID: "74854950521", TitleRomaji: "Child of the Sheath", Year: 2020, Format: "MANGA"}},
	}
	if id := svc.resolveID(ctx, stored, mu); id != "74854950521" {
		t.Fatalf("resolved %q, want the English title match", id)
	}
}

type countingProvider struct {
	*fakeProvider
	mu    sync.Mutex
	calls map[catalog.Call]int64
}

func (c *countingProvider) Count(k catalog.Call, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[catalog.Call]int64{}
	}
	c.calls[k] += n
}

func (c *countingProvider) TakeCalls() map[catalog.Call]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	return out
}

func TestTrackerCallsAddUpByReasonAndKind(t *testing.T) {
	svc, ctx := openTestJobs(t)
	mal, mu := &countingProvider{fakeProvider: newFake(catalog.MALProvider)}, &countingProvider{fakeProvider: newFake(catalog.MangaUpdatesProvider)}
	svc.catalog.providers[catalog.MALProvider], svc.catalog.providers[catalog.MangaUpdatesProvider] = mal, mu
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO tracker_calls (provider, day, kind, reason, count) VALUES ('mal', '2020-01-01', 'Search', 'Search', 100)`); err != nil {
		t.Fatal(err)
	}
	search := catalog.Call{Kind: "Search", Reason: "Search"}
	mal.Count(search, 5)
	svc.FlushRequests(ctx)
	mal.Count(search, 3)
	mal.Count(catalog.Call{Kind: "Details", Reason: "Title page"}, 1)
	mu.Count(catalog.Call{Kind: "List", Reason: "Tracker sync"}, 2)
	svc.CountImage("https://cdn.mangaupdates.com/image/i1.png", "Cover proxy")
	svc.CountImage("https://example.test/cover.jpg", "Cover proxy")
	stats := svc.TrackerRequestStats(ctx)
	want := []TrackerRequests{
		{Label: "MyAnimeList", API: RequestCounts{9, 9, 109}},
		{Label: "MangaUpdates", API: RequestCounts{2, 2, 2}, Images: RequestCounts{1, 1, 1}},
		{Label: "All trackers", API: RequestCounts{11, 11, 111}, Images: RequestCounts{1, 1, 1}, All: true},
	}
	if !slices.Equal(stats, want) {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	details := fmt.Sprint(svc.TrackerCallDetails(ctx))
	if wantDetails := "[{All trackers [{Search Search {8 8 108}} {Tracker sync List {2 2 2}} {Cover proxy Image {1 1 1}} {Title page Details {1 1 1}}] {12 12 112}} " +
		"{MyAnimeList [{Search Search {8 8 108}} {Title page Details {1 1 1}}] {9 9 109}} " +
		"{MangaUpdates [{Tracker sync List {2 2 2}} {Cover proxy Image {1 1 1}}] {3 3 3}}]"; details != wantDetails {
		t.Fatalf("details = %s\nwant      %s", details, wantDetails)
	}
}

func TestDefaultProviderFollowsFirstConnectionAndBlocks(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	if got := svc.DefaultProvider(ctx); got != catalog.AniListProvider {
		t.Fatalf("no accounts: default = %q", got)
	}
	connect(t, svc, ctx, catalog.MangaUpdatesProvider, time.Now().Add(-time.Hour))
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	if got := svc.DefaultProvider(ctx); got != catalog.MangaUpdatesProvider {
		t.Fatalf("default = %q, want earliest-connected mangaupdates", got)
	}
	auth.FromContext(ctx).BlockedProviders = []string{catalog.MangaUpdatesProvider}
	if got := svc.DefaultProvider(ctx); got != catalog.KitsuProvider {
		t.Fatalf("default with mangaupdates blocked = %q, want kitsu", got)
	}
	if _, _, err := svc.SearchManga(ctx, catalog.MangaUpdatesProvider, "x", 5, catalog.SearchFilter{}); err == nil {
		t.Fatal("blocked provider searchable")
	}
	if _, err := svc.Provider(ctx, catalog.MALProvider); err == nil {
		t.Fatal("MAL usable without a client id")
	}
}

func TestSwitchTitleMetadataKeepsMappings(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	kitsu := newFake(catalog.KitsuProvider)
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	kitsu.works["7"] = catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "7", TitleRomaji: "Berserk (Kitsu)"}
	work := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", TitleRomaji: "Berserk", ExternalIDs: map[string]string{catalog.MALProvider: "10"}}
	titleID, stored := seedTitle(t, svc, ctx, work, 1, 0)
	if _, err := svc.catalog.repo.UpsertMatch(ctx, catalog.Match{CatalogMangaID: stored.ID, SourceURL: "https://src/berserk", ChaptersFound: 3}); err != nil {
		t.Fatal(err)
	}

	if err := svc.SwitchTitleMetadata(ctx, titleID, catalog.KitsuProvider, "7"); err != nil {
		t.Fatal(err)
	}
	m, ok := svc.titleWork(ctx, titleID)
	if !ok || m.Provider != catalog.KitsuProvider || m.ProviderID != "7" {
		t.Fatalf("title work = %+v", m)
	}
	maps := svc.mappings(ctx, m)
	if maps[catalog.AniListProvider].ProviderID != "1" || maps[catalog.MALProvider].ProviderID != "10" {
		t.Fatalf("mappings after switch = %+v", maps)
	}
	if matches, _ := svc.ListMatches(ctx, m.ID); len(matches) != 1 || matches[0].SourceURL != "https://src/berserk" {
		t.Fatalf("source matches after switch = %+v", matches)
	}
	if inLib, _ := svc.TitlesByProvider(ctx, catalog.AniListProvider); inLib["1"] != titleID {
		t.Fatalf("anilist id no longer maps to the title: %v", inLib)
	}
}

func TestMergeMangaDedupesAcrossProviders(t *testing.T) {
	a := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", ExternalIDs: map[string]string{catalog.MALProvider: "10"}}
	b := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "2"}
	c := catalog.Manga{Provider: catalog.MALProvider, ProviderID: "10"}
	self := catalog.Manga{Provider: catalog.MALProvider, ProviderID: "99"}
	out := mergeManga([][]catalog.Manga{{b, a}, {c, self}}, map[string]bool{"mal:99": true}, 0)
	if len(out) != 2 || out[0].ProviderID != "1" || out[1].ProviderID != "2" {
		t.Fatalf("merged = %+v, want work both lists agree on first, excluded one dropped", out)
	}
}

func TestTrackerSuggestionsFollowPickedProvider(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	kitsu, mu := newFake(catalog.KitsuProvider), newFake(catalog.MangaUpdatesProvider)
	svc.catalog.providers[catalog.KitsuProvider], svc.catalog.providers[catalog.MangaUpdatesProvider] = kitsu, mu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	connect(t, svc, ctx, catalog.MangaUpdatesProvider, time.Now())
	kitsu.entries["1"] = catalog.Entry{Status: "CURRENT"}
	mu.entries["2"] = catalog.Entry{Status: "CURRENT"}

	if got := svc.TrackerSuggestions(ctx, catalog.KitsuProvider); len(got) != 1 || got[0].Provider != catalog.KitsuProvider {
		t.Fatalf("kitsu suggestions = %+v", got)
	}
	if got := svc.TrackerSuggestions(ctx, ""); len(got) != 2 {
		t.Fatalf("unscoped suggestions = %+v", got)
	}
}

func TestProviderSuggestionsSeedFromRemoteList(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	al := newFake(catalog.AniListProvider)
	svc.catalog.providers[catalog.AniListProvider] = al
	al.entries["1"] = catalog.Entry{Status: "CURRENT"}
	al.entries["2"] = catalog.Entry{Status: "PLANNING"}
	al.search = []catalog.Manga{
		{Provider: catalog.AniListProvider, ProviderID: "2"},
		{Provider: catalog.AniListProvider, ProviderID: "3"},
	}
	if _, _, ok := svc.ProviderSuggestions(ctx, catalog.AniListProvider, false); ok {
		t.Fatal("suggestions without a connected account")
	}
	connect(t, svc, ctx, catalog.AniListProvider, time.Now())
	got, _, ok := svc.ProviderSuggestions(ctx, catalog.AniListProvider, false)
	if !ok || len(got) != 1 || got[0].ProviderID != "3" {
		t.Fatalf("suggestions = %+v, want only the unlisted recommendation", got)
	}
}

func TestSyncPushesReadingDatesOnlyWhenRemoteHasNone(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := newFake(catalog.KitsuProvider)
	kitsu.dates = true
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	work := catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Done", Status: "FINISHED"}
	titleID, _ := seedTitle(t, svc, ctx, work, 2, 2)
	if _, err := svc.db.ExecContext(ctx, `UPDATE chapter_read_progress SET started_at = '2024-03-01 10:00:00', completed_at = '2024-03-05 10:00:00' WHERE chapter_id IN (SELECT id FROM chapters WHERE title_id = ? AND number_main = 1)`, titleID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.ExecContext(ctx, `UPDATE chapter_read_progress SET started_at = '2024-03-06 10:00:00', completed_at = '2024-03-09 10:00:00' WHERE chapter_id IN (SELECT id FROM chapters WHERE title_id = ? AND number_main = 2)`, titleID); err != nil {
		t.Fatal(err)
	}
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := kitsu.savesCopy(); len(got) != 1 || got[0] != "9:2:COMPLETED:2024-03-01:2024-03-09" {
		t.Fatalf("saves = %v, want first-start and last-completion dates", got)
	}
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := kitsu.savesCopy(); len(got) != 1 {
		t.Fatalf("dates re-pushed though remote has them: %v", got)
	}
}

type cappedTracker struct {
	*fakeProvider
	limit, lists, gets int
}

func (c *cappedTracker) ListEntries(ctx context.Context) ([]catalog.Entry, error) {
	c.lists++
	return c.fakeProvider.ListEntries(ctx)
}

func (c *cappedTracker) GetEntry(ctx context.Context, id string) (catalog.Entry, bool, error) {
	c.gets++
	return c.fakeProvider.GetEntry(ctx, id)
}

func (c *cappedTracker) SaveEntry(ctx context.Context, id string, u catalog.EntryUpdate) error {
	if c.limit > 0 {
		u.Progress = min(u.Progress, c.limit)
	}
	return c.fakeProvider.SaveEntry(ctx, id, u)
}

func (c *cappedTracker) want(t *testing.T, lists, gets, saves int) {
	t.Helper()
	if c.lists != lists || c.gets != gets || len(c.savesCopy()) != saves {
		t.Fatalf("list downloads = %d, entry reads = %d, saves = %v; want %d, %d and %d", c.lists, c.gets, c.savesCopy(), lists, gets, saves)
	}
}

func TestScheduledSyncCallsTrackersOnlyForChanges(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := &cappedTracker{fakeProvider: newFake(catalog.KitsuProvider), limit: 2}
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Capped", Status: "FINISHED"}, 3, 3)
	seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "10", TitleRomaji: "Other"}, 1, 0)
	seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "11", TitleRomaji: "Third"}, 1, 0)
	sync := func(full bool) {
		t.Helper()
		if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, full, nil); err != nil {
			t.Fatal(err)
		}
	}
	sync(false)
	kitsu.want(t, 1, 0, 3)
	sync(false)
	kitsu.want(t, 1, 0, 3)
	if _, err := svc.db.ExecContext(ctx, `UPDATE chapter_read_progress SET completed = 0 WHERE chapter_id IN (SELECT id FROM chapters WHERE title_id = ? AND number_main = 3)`, titleID); err != nil {
		t.Fatal(err)
	}
	sync(false)
	kitsu.want(t, 1, 1, 3)
	sync(false)
	kitsu.want(t, 1, 1, 3)
	sync(true)
	kitsu.want(t, 2, 1, 3)
}

func TestPushesSettleSoSyncsStayQuiet(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := &cappedTracker{fakeProvider: newFake(catalog.KitsuProvider)}
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Read", Status: "RELEASING"}, 4, 1)
	kitsu.entries["9"] = catalog.Entry{Progress: 0, Status: "CURRENT"}
	uctx := asUser(svc, ctx, t)
	read := func(n int) {
		t.Helper()
		if _, err := svc.db.ExecContext(ctx, `INSERT INTO chapter_read_progress (user_id, chapter_id, completed) SELECT 1, id, 1 FROM chapters WHERE title_id = ? AND number_main = ?`, titleID, n); err != nil {
			t.Fatal(err)
		}
		svc.PushTrackers(uctx, 1, titleID)
		svc.FlushPushes(time.Second)
	}
	read(2)
	kitsu.want(t, 0, 1, 1)
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, false, nil); err != nil {
		t.Fatal(err)
	}
	kitsu.want(t, 0, 1, 1)
	prefs := svc.TrackerAccounts(uctx, 1)[0].TrackerPrefs
	prefs.Exclusive = true
	if err := svc.SetTrackerPrefs(uctx, catalog.KitsuProvider, prefs); err != nil {
		t.Fatal(err)
	}
	read(3)
	kitsu.want(t, 0, 2, 2)
	read(4)
	kitsu.want(t, 0, 2, 3)
	if got := kitsu.savesCopy(); got[2] != "9:4:" {
		t.Fatalf("exclusive push = %q, want progress from the kept entry", got[2])
	}
}

func TestLinkedTitleSyncsOnlyWhenAsked(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := newFake(catalog.KitsuProvider)
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	work := catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Linked", Status: "RELEASING"}
	titleID, _ := seedTitle(t, svc, ctx, work, 3, 1)
	if _, err := svc.db.ExecContext(ctx, `DELETE FROM tracker_title_sync`); err != nil {
		t.Fatal(err)
	}
	uctx := asUser(svc, ctx, t)
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, false, nil); err != nil {
		t.Fatal(err)
	}
	if links, _ := svc.TitleLinks(uctx, titleID); len(kitsu.savesCopy()) != 0 || len(links) != 1 || links[0].Syncing {
		t.Fatalf("linked title synced without consent: saves=%v links=%+v", kitsu.savesCopy(), links)
	}
	if err := svc.SyncTrackersTitle(uctx, titleID, catalog.KitsuProvider); err != nil || len(kitsu.savesCopy()) != 1 {
		t.Fatalf("one-off sync: err=%v saves=%v", err, kitsu.savesCopy())
	}
	if err := svc.SetProgressiveSync(uctx, titleID, catalog.KitsuProvider, true); err != nil {
		t.Fatal(err)
	}
	if links, _ := svc.TitleLinks(uctx, titleID); !links[0].Syncing {
		t.Fatalf("progressive sync not recorded: %+v", links)
	}
	if err := svc.SetProgressiveSync(uctx, titleID, catalog.KitsuProvider, false); err != nil {
		t.Fatal(err)
	}
	if got := svc.progressiveTrackers(ctx, 1, titleID); len(got) != 0 {
		t.Fatalf("progressive sync still on: %v", got)
	}
}

func TestTrackerPrefsDriveLinkingSyncAndRelated(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := newFake(catalog.KitsuProvider)
	kitsu.search = []catalog.Manga{{Provider: catalog.KitsuProvider, ProviderID: "50", TitleRomaji: "Other"}}
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	uctx := asUser(svc, ctx, t)
	native, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Native"}, 1, 0)
	_, unlinked := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", TitleRomaji: "Unlinked"}, 1, 0)
	_, notOn := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "2", TitleRomaji: "Not There"}, 1, 0)
	_, guessed := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "3", TitleRomaji: "Guessed"}, 1, 0)
	if _, err := svc.db.ExecContext(ctx, `DELETE FROM tracker_title_sync`); err != nil {
		t.Fatal(err)
	}
	_ = svc.catalog.repo.SetExternalID(ctx, notOn.ID, catalog.ExternalID{Provider: catalog.KitsuProvider, Method: "user"})
	_ = svc.catalog.repo.SetExternalID(ctx, guessed.ID, catalog.ExternalID{Provider: catalog.KitsuProvider, ProviderID: "77", Method: "title_match"})

	if err := svc.SetTrackerPrefs(uctx, catalog.KitsuProvider, TrackerPrefs{AutoProgressive: true, Relations: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, false, nil); err != nil {
		t.Fatal(err)
	}
	if id := svc.confirmedID(ctx, unlinked, catalog.KitsuProvider); id != "" || len(svc.mappings(ctx, unlinked)) != 1 {
		t.Fatalf("auto link off still looked titles up: %v", svc.mappings(ctx, unlinked))
	}
	if got := svc.progressiveTrackers(ctx, 1, native); len(got) != 1 {
		t.Fatalf("auto progressive not applied to a linked title: %v", got)
	}
	if err := svc.SetProgressiveSync(uctx, native, catalog.KitsuProvider, false); err != nil {
		t.Fatal(err)
	}
	if got := svc.progressiveTrackers(ctx, 1, native); len(got) != 0 {
		t.Fatalf("explicit off lost to the auto default: %v", got)
	}
	nativeWork, _ := svc.titleWork(ctx, native)
	if _, recs, _, _ := svc.RelatedManga(uctx, nativeWork.ID, 10); len(recs) != 0 {
		t.Fatalf("recommendations used though turned off: %v", recs)
	}

	titles, err := svc.UnlinkedTitles(ctx, catalog.KitsuProvider)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ut := range titles {
		names = append(names, fmt.Sprintf("%s:%v", ut.DisplayTitle, ut.Suggested))
		if ut.Provider == "" || ut.Provider == catalog.KitsuProvider {
			t.Fatalf("unlinked title without its metadata backend: %+v", ut)
		}
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "t:false,t:true" {
		t.Fatalf("unlinked titles = %v, want the guessed and the unlinked one only", names)
	}

	if err := svc.SetTrackerPrefs(uctx, catalog.KitsuProvider, TrackerPrefs{AutoLink: true, Recommendations: true}); err != nil {
		t.Fatal(err)
	}
	if _, recs, _, _ := svc.RelatedManga(uctx, nativeWork.ID, 10); len(recs) != 1 {
		t.Fatalf("recommendations missing once turned on: %v", recs)
	}
}

type recommendingProvider struct {
	*fakeProvider
	lists   atomic.Int32
	related int
}

func (r *recommendingProvider) ListEntries(ctx context.Context) ([]catalog.Entry, error) {
	r.lists.Add(1)
	return r.fakeProvider.ListEntries(ctx)
}
func (r *recommendingProvider) Related(context.Context, string, int) ([]catalog.Manga, []catalog.Manga, error) {
	r.related++
	return nil, []catalog.Manga{{Provider: catalog.MangaUpdatesProvider, ProviderID: "8", TitleRomaji: "Stub", IsAdult: true}}, nil
}

func TestSuggestionsReuseStoredListsAndRecommendations(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	mu := &recommendingProvider{fakeProvider: newFake(catalog.MangaUpdatesProvider)}
	mu.entries["1"] = catalog.Entry{Status: "CURRENT"}
	svc.catalog.providers[catalog.MangaUpdatesProvider] = mu
	connect(t, svc, ctx, catalog.MangaUpdatesProvider, time.Now())
	if _, err := svc.catalog.repo.UpsertManga(ctx, catalog.Manga{Provider: catalog.MangaUpdatesProvider, ProviderID: "8", TitleRomaji: "Known", Genres: []string{"Drama"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.catalog.repo.UpsertManga(ctx, catalog.Manga{Provider: catalog.MangaUpdatesProvider, ProviderID: "1", TitleRomaji: "Listed", CoverImage: "https://c/1.jpg", Genres: []string{"Drama"}}); err != nil {
		t.Fatal(err)
	}
	if listed := svc.TrackerSuggestions(ctx, catalog.MangaUpdatesProvider); len(listed) != 1 || listed[0].CoverImage != "https://c/1.jpg" {
		t.Fatalf("list entry without a cover should use the stored one: %+v", listed)
	}
	got, _, ok := svc.ProviderSuggestions(ctx, catalog.MangaUpdatesProvider, false)
	if !ok || len(got) != 1 || got[0].TitleRomaji != "Known" || got[0].IsAdult {
		t.Fatalf("suggestions = %+v, want the stub filled from the stored record", got)
	}
	if mu.lists.Load() != 1 || mu.related != 1 {
		t.Fatalf("list downloads = %d, Related calls = %d; want 1 and 1", mu.lists.Load(), mu.related)
	}
	svc.invalidateRecs(1)
	svc.listMu.Lock()
	svc.listCache = nil
	svc.listMu.Unlock()
	if got, _, _ := svc.ProviderSuggestions(ctx, catalog.MangaUpdatesProvider, false); len(got) != 1 || mu.lists.Load() != 1 || mu.related != 1 {
		t.Fatalf("after a restart: %d suggestions, %d list downloads, %d Related calls; want the stored copies", len(got), mu.lists.Load(), mu.related)
	}
	if err := svc.DisconnectTracker(ctx, 1, catalog.MangaUpdatesProvider); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := svc.ProviderSuggestions(ctx, catalog.MangaUpdatesProvider, false); ok {
		t.Fatal("a disconnected tracker must not serve its cached list")
	}
}

func TestListCacheRefreshesInTheBackground(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	kitsu := &recommendingProvider{fakeProvider: newFake(catalog.KitsuProvider)}
	kitsu.entries["1"] = catalog.Entry{Status: "CURRENT"}
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	if got := svc.TrackerSuggestions(ctx, catalog.KitsuProvider); len(got) != 1 {
		t.Fatalf("first load = %+v", got)
	}
	kitsu.mu.Lock()
	delete(kitsu.entries, "1")
	kitsu.mu.Unlock()
	if got := svc.TrackerSuggestions(ctx, catalog.KitsuProvider); len(got) != 1 || kitsu.lists.Load() != 1 {
		t.Fatal("a fresh list must be served from memory")
	}
	svc.listMu.Lock()
	for k, c := range svc.listCache {
		c.fetched = time.Now().Add(-listFresh)
		svc.listCache[k] = c
	}
	svc.listMu.Unlock()
	if got := svc.TrackerSuggestions(ctx, catalog.KitsuProvider); len(got) != 1 {
		t.Fatal("an aging list is still served right away")
	}
	waitFor(t, func() bool { return len(svc.TrackerSuggestions(ctx, catalog.KitsuProvider)) == 0 }, "the background refresh should drop the removed entry")
}

func waitFor(t *testing.T, ok func() bool, msg string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal(msg)
}

type favProvider struct {
	*fakeProvider
	favs map[string]bool
	fail bool
}

func (f *favProvider) Favourites(context.Context) ([]string, error) {
	var out []string
	for id, on := range f.favs {
		if on {
			out = append(out, id)
		}
	}
	return out, nil
}
func (f *favProvider) IsFavourite(_ context.Context, id string) (bool, error) { return f.favs[id], nil }
func (f *favProvider) ToggleFavourite(_ context.Context, id string) error {
	if f.fail {
		return errors.New("down")
	}
	f.favs[id] = !f.favs[id]
	return nil
}

func TestFavouritesFlowFromTheSourceSide(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	kitsu := &favProvider{fakeProvider: newFake(catalog.KitsuProvider), favs: map[string]bool{"9": true}}
	al := &favProvider{fakeProvider: newFake(catalog.AniListProvider), favs: map[string]bool{}}
	svc.catalog.providers[catalog.KitsuProvider], svc.catalog.providers[catalog.AniListProvider] = kitsu, al
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	connect(t, svc, ctx, catalog.AniListProvider, time.Now().Add(time.Minute))
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Fav", ExternalIDs: map[string]string{catalog.AniListProvider: "1"}}, 1, 0)
	local := func() bool {
		var n int
		_ = svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_favourites WHERE user_id = 1 AND title_id = ?`, titleID).Scan(&n)
		return n > 0
	}
	sync := func(name string) { _ = svc.runTrackerSync(ctx, 1, name, false, nil) }

	sync(catalog.KitsuProvider)
	if local() || kitsu.favs["9"] {
		t.Fatalf("Kaodoku leads: local=%v remote=%v, want Kaodoku's unfavourited state pushed", local(), kitsu.favs["9"])
	}
	for _, name := range []string{catalog.KitsuProvider, catalog.AniListProvider} {
		if err := svc.SetTrackerPrefs(ctx, name, TrackerPrefs{Source: SourceTracker}); err != nil {
			t.Fatal(err)
		}
	}
	kitsu.favs["9"] = true
	sync(catalog.KitsuProvider)
	sync(catalog.AniListProvider)
	if !local() || al.favs["1"] {
		t.Fatalf("Kitsu leads first: local=%v anilist=%v, want Kitsu's favourite and AniList untouched", local(), al.favs["1"])
	}
	kitsu.favs["9"] = false
	sync(catalog.KitsuProvider)
	if local() {
		t.Fatal("a removal on the source should be pulled")
	}
}

func TestSearchKeepsStoredRecords(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	mu := newFake(catalog.MangaUpdatesProvider)
	svc.catalog.providers[catalog.MangaUpdatesProvider] = mu
	full := catalog.Manga{Provider: catalog.MangaUpdatesProvider, ProviderID: "55", TitleRomaji: "Solo", Authors: []string{"Chugong"}, Synonyms: []string{"Na Honjaman"}, Status: "FINISHED"}
	stored, err := svc.catalog.repo.UpsertManga(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	mu.search = []catalog.Manga{{Provider: catalog.MangaUpdatesProvider, ProviderID: "55", TitleRomaji: "Solo", Status: "RELEASING"}, {Provider: catalog.MangaUpdatesProvider, ProviderID: "56", TitleRomaji: "New"}}
	got, _, err := svc.SearchManga(ctx, catalog.MangaUpdatesProvider, "solo", 5, catalog.SearchFilter{})
	if err != nil || len(got) != 2 || got[0].ID != stored.ID || got[1].ID == 0 {
		t.Fatalf("results = %+v, %v", got, err)
	}
	after, _ := svc.catalog.repo.GetManga(ctx, stored.ID)
	if after.Status != "FINISHED" || len(after.Authors) != 1 || len(after.Synonyms) != 1 {
		t.Fatalf("a partial search result overwrote the stored record: %+v", after)
	}
	mu.search = nil
	if again, _, _ := svc.SearchManga(ctx, catalog.MangaUpdatesProvider, "solo", 5, catalog.SearchFilter{}); len(again) != 2 {
		t.Fatal("a repeated search must come from the cache")
	}
	if other, _, _ := svc.SearchManga(ctx, catalog.MangaUpdatesProvider, "solo", 5, catalog.SearchFilter{Page: 2}); len(other) != 0 {
		t.Fatal("another page must not share the cached results")
	}
}

type barrierProvider struct {
	*fakeProvider
	started *sync.WaitGroup
	outage  string
	calls   atomic.Int32
}

func (b *barrierProvider) Outage() string { return b.outage }
func (b *barrierProvider) ListEntries(ctx context.Context) ([]catalog.Entry, error) {
	b.calls.Add(1)
	b.started.Done()
	done := make(chan struct{})
	go func() { b.started.Wait(); close(done) }()
	select {
	case <-done:
		return b.fakeProvider.ListEntries(ctx)
	case <-time.After(2 * time.Second):
		return nil, errors.New("lists were downloaded one after another")
	}
}

func TestTrackerListsDownloadSideBySideAndSkipOutages(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	var started sync.WaitGroup
	started.Add(2)
	kitsu := &barrierProvider{fakeProvider: newFake(catalog.KitsuProvider), started: &started}
	mu := &barrierProvider{fakeProvider: newFake(catalog.MangaUpdatesProvider), started: &started}
	down := &barrierProvider{fakeProvider: newFake(catalog.AniListProvider), started: &started, outage: "maintenance"}
	kitsu.entries["1"], mu.entries["2"], down.entries["3"] = catalog.Entry{Status: "CURRENT"}, catalog.Entry{Status: "CURRENT"}, catalog.Entry{Status: "CURRENT"}
	svc.catalog.providers[catalog.KitsuProvider], svc.catalog.providers[catalog.MangaUpdatesProvider], svc.catalog.providers[catalog.AniListProvider] = kitsu, mu, down
	for _, p := range []string{catalog.KitsuProvider, catalog.MangaUpdatesProvider, catalog.AniListProvider} {
		connect(t, svc, ctx, p, time.Now())
	}
	if got := svc.TrackerSuggestions(ctx, ""); len(got) != 2 {
		t.Fatalf("suggestions = %+v, want both reachable lists", got)
	}
	if down.calls.Load() != 0 {
		t.Fatal("a tracker in outage must not be asked for its list")
	}
}

type pingedProvider struct {
	*fakeProvider
	outage string
	pings  int
}

func (p *pingedProvider) Outage() string { return p.outage }
func (p *pingedProvider) Trending(context.Context, int) ([]catalog.Manga, error) {
	p.pings++
	return nil, nil
}

func TestOnlyTrackersInAnOutageArePingedWithBackoff(t *testing.T) {
	svc, ctx := openTestJobs(t)
	down := &pingedProvider{fakeProvider: newFake(catalog.KitsuProvider), outage: "down"}
	up := &pingedProvider{fakeProvider: newFake(catalog.MangaUpdatesProvider)}
	svc.catalog.providers[catalog.KitsuProvider], svc.catalog.providers[catalog.MangaUpdatesProvider] = down, up
	retries := map[string]outageRetry{}
	start := time.Now()
	var at []int
	for m := range 62 {
		before := down.pings
		svc.probeOutages(ctx, retries, start.Add(time.Duration(m)*time.Minute))
		if down.pings > before {
			at = append(at, m)
		}
	}
	if fmt.Sprint(at) != "[1 3 7 15 31 61]" || up.pings != 0 {
		t.Fatalf("pinged the down tracker at minutes %v and the working one %d times", at, up.pings)
	}
}

func TestPushesWaitForTheReadingSessionToEnd(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := newFake(catalog.KitsuProvider)
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Read", Status: "RELEASING"}, 5, 2)
	kitsu.entries["9"] = catalog.Entry{Progress: 4, Status: "CURRENT"}
	uctx := asUser(svc, ctx, t)
	svc.PushTrackers(uctx, 1, titleID)
	svc.PushTrackersExact(uctx, 1, titleID)
	svc.PushTrackers(uctx, 1, titleID)
	if got := kitsu.savesCopy(); len(got) != 0 {
		t.Fatalf("pushed before the session ended: %v", got)
	}
	svc.FlushPushes(time.Second)
	svc.FlushPushes(time.Second)
	if got := kitsu.savesCopy(); len(got) != 1 || got[0] != "9:2:" {
		t.Fatalf("saves = %v, want one exact push of the final progress", got)
	}
}

type refreshProvider struct {
	*fakeProvider
	gets  []string
	vocab int
}

func (r *refreshProvider) Get(ctx context.Context, id string) (catalog.Manga, []catalog.Relation, error) {
	r.gets = append(r.gets, id)
	return r.fakeProvider.Get(ctx, id)
}
func (r *refreshProvider) TagVocabulary(context.Context) ([]catalog.ContentTag, error) {
	r.vocab++
	return []catalog.ContentTag{{Name: "Drama", Kind: "genre"}}, nil
}

func TestCatalogRefreshSkipsFinishedWorksAndTagsUntilDue(t *testing.T) {
	svc, ctx := openTestJobs(t)
	al := &refreshProvider{fakeProvider: newFake(catalog.AniListProvider)}
	svc.catalog.providers[catalog.AniListProvider] = al
	for id, status := range map[string]string{"1": "FINISHED", "2": "RELEASING"} {
		m := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: id, TitleRomaji: "W" + id, Status: status}
		al.works[id] = m
		seedTitle(t, svc, ctx, m, 1, 0)
	}
	for range 2 {
		if err := svc.runCatalogRefresh(ctx, noopProgress{}); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(al.gets) != "[2 2]" || al.vocab != 1 {
		t.Fatalf("refreshed works %v and tag lists %d times, want only the releasing work and the tags once", al.gets, al.vocab)
	}
}

func TestMatchingRefetchesOnlyAStaleRecord(t *testing.T) {
	svc, ctx := openTestJobs(t)
	al := &refreshProvider{fakeProvider: newFake(catalog.AniListProvider)}
	svc.catalog.providers[catalog.AniListProvider] = al
	kitsu := newFake(catalog.KitsuProvider)
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	m := catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", TitleRomaji: "W"}
	al.works["1"] = m
	stored, err := svc.catalog.repo.UpsertManga(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	svc.resolveID(ctx, stored, kitsu)
	if _, err := svc.db.ExecContext(ctx, `DELETE FROM catalog_external_ids; UPDATE catalog_manga SET updated_at = '2020-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	old, _ := svc.catalog.GetManga(ctx, stored.ID)
	svc.resolveID(ctx, old, kitsu)
	if fmt.Sprint(al.gets) != "[1]" {
		t.Fatalf("fetched the stored record %v, want only once it was stale", al.gets)
	}
}

func TestRatingsFollowEachTrackersLeadAndOrder(t *testing.T) {
	svc, ctx := openTestJobs(t)
	al, kitsu, mu := newFake(catalog.AniListProvider), newFake(catalog.KitsuProvider), newFake(catalog.MangaUpdatesProvider)
	for i, f := range []*fakeProvider{al, kitsu, mu} {
		svc.catalog.providers[f.name] = f
		connect(t, svc, ctx, f.name, time.Now().Add(time.Duration(i)*time.Minute))
		f.entries["1"] = catalog.Entry{Progress: 1, Status: "CURRENT", Score: []int{8, 6, 3}[i]}
	}
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", TitleRomaji: "Rated", ExternalIDs: map[string]string{catalog.KitsuProvider: "1", catalog.MangaUpdatesProvider: "1"}}, 3, 1)
	uctx := asUser(svc, ctx, t)
	for _, name := range []string{catalog.AniListProvider, catalog.KitsuProvider} {
		if err := svc.SetTrackerPrefs(uctx, name, TrackerPrefs{Source: SourceTracker}); err != nil {
			t.Fatal(err)
		}
	}
	sync := func(names ...string) {
		for _, name := range names {
			if err := svc.runTrackerSync(ctx, 1, name, false, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	sync(catalog.KitsuProvider, catalog.AniListProvider, catalog.MangaUpdatesProvider)
	if got := svc.TitleRating(uctx, titleID); got.Rating != 8 || got.From != "AniList" || len(al.savesCopy())+len(kitsu.savesCopy()) != 0 {
		t.Fatalf("rating = %+v, tracker-led saves %v %v; want AniList's 8 and nothing sent to the leaders", got, al.savesCopy(), kitsu.savesCopy())
	}
	waitFor(t, func() bool { saves := mu.savesCopy(); return len(saves) > 0 && saves[len(saves)-1] == "1:-1::score=8" }, "MangaUpdates should end at AniList's 8")
	for _, save := range mu.savesCopy() {
		if !strings.HasPrefix(save, "1:-1::score=") {
			t.Fatalf("kaodoku-led save %q carried more than the rating", save)
		}
	}
	if err := svc.ReorderTrackers(uctx, []string{catalog.KitsuProvider, catalog.AniListProvider, catalog.MangaUpdatesProvider}); err != nil {
		t.Fatal(err)
	}
	sync(catalog.AniListProvider)
	if got := svc.TitleRating(uctx, titleID); got.Rating != 6 || got.From != "Kitsu" {
		t.Fatalf("after moving Kitsu up: rating %+v, want Kitsu's 6", got)
	}
	waitFor(t, func() bool { saves := mu.savesCopy(); return saves[len(saves)-1] == "1:-1::score=6" }, "the new rating should reach MangaUpdates right away")
}

func TestKaodokuLedRatingsPushOnlyWhatWasSet(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := newFake(catalog.KitsuProvider)
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	kitsu.entries["9"] = catalog.Entry{Progress: 1, Status: "CURRENT", Score: 7}
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Rated"}, 3, 1)
	uctx := asUser(svc, ctx, t)
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, false, nil); err != nil || len(kitsu.savesCopy()) != 0 {
		t.Fatalf("an unrated title cleared the tracker's rating: %v %v", kitsu.savesCopy(), err)
	}
	if _, err := svc.db.ExecContext(ctx, `DELETE FROM tracker_title_sync`); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetTrackerPrefs(uctx, catalog.KitsuProvider, TrackerPrefs{RatingSync: "on"}); err != nil {
		t.Fatal(err)
	}
	for i, r := range []int{9, 0} {
		if err := svc.SetTitleRating(uctx, titleID, r); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return len(kitsu.savesCopy()) == i+1 }, "a rating should be sent right away")
	}
	if got := fmt.Sprint(kitsu.savesCopy()); got != "[9:-1::score=9 9:-1::score=-1]" {
		t.Fatalf("saves = %s, want the rating set then cleared without progress", got)
	}
}

func TestMissingRatingsAreAskedUntilAnswered(t *testing.T) {
	svc, ctx := openTestJobs(t)
	kitsu := newFake(catalog.KitsuProvider)
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	kitsu.entries["9"] = catalog.Entry{Progress: 1, Status: "CURRENT"}
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Rated"}, 3, 1)
	uctx := asUser(svc, ctx, t)
	if err := svc.SetTrackerPrefs(uctx, catalog.KitsuProvider, TrackerPrefs{Source: SourceTracker}); err != nil {
		t.Fatal(err)
	}
	if err := svc.storeRating(ctx, 1, titleID, 7); err != nil {
		t.Fatal(err)
	}
	sync := func() {
		if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	missing := func() bool {
		list, _ := svc.MissingRatings(uctx, catalog.KitsuProvider)
		return len(svc.TitleRating(uctx, titleID).Missing) == 1 && len(list) == 1
	}
	sync()
	if !missing() {
		t.Fatal("an unrated title on the source should be flagged")
	}
	if err := svc.AnswerMissingRating(uctx, titleID, catalog.KitsuProvider, true); err != nil || fmt.Sprint(kitsu.savesCopy()) != "[9:-1::score=7]" || missing() {
		t.Fatalf("send: saves %v, err %v, still missing %v", kitsu.savesCopy(), err, missing())
	}
	kitsu.entries["9"] = catalog.Entry{Progress: 1, Status: "CURRENT"}
	sync()
	if err := svc.AnswerMissingRating(uctx, titleID, catalog.KitsuProvider, false); err != nil || missing() {
		t.Fatalf("keep should stop asking: %v", err)
	}
	if err := svc.SetTitleRating(uctx, titleID, 8); err != nil || !missing() {
		t.Fatalf("a changed rating should ask again: %v", err)
	}
}
