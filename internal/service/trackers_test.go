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
	f.saves = append(f.saves, save)
	e := f.entries[id]
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

	err := svc.runTrackerSync(ctx, 1, catalog.AniListProvider, nil)
	if err == nil || !strings.Contains(err.Error(), "reconnect AniList") {
		t.Fatalf("sync error = %v", err)
	}
	accounts := svc.TrackerAccounts(ctx, 1)
	if len(accounts) != 1 || accounts[0].Provider != catalog.KitsuProvider {
		t.Fatalf("accounts after revoke = %+v, want only kitsu", accounts)
	}
}

func TestTrackersConvergeThroughLibrary(t *testing.T) {
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
	seedTitle(t, svc, ctx, work, 8, 0)
	mal.entries["10"] = catalog.Entry{Progress: 5, Status: "CURRENT"}

	if err := svc.runTrackerSync(ctx, 1, catalog.MALProvider, nil); err != nil {
		t.Fatalf("mal sync: %v", err)
	}
	if err := svc.runTrackerSync(ctx, 1, catalog.AniListProvider, nil); err != nil {
		t.Fatalf("anilist sync: %v", err)
	}
	if got := anilist.savesCopy(); len(got) != 1 || got[0] != "1:5:CURRENT" {
		t.Fatalf("anilist saves = %v, want progress pulled from MAL pushed up", got)
	}
	if got := mal.savesCopy(); len(got) != 0 {
		t.Fatalf("mal saves = %v, want none (already current)", got)
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
	if err := svc.runTrackerSync(ctx, 1, catalog.MangaUpdatesProvider, nil); err != nil {
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
	kitsu := newFake(catalog.KitsuProvider)
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	kitsu.entries["1"] = catalog.Entry{Status: "CURRENT"}
	kitsu.entries["2"] = catalog.Entry{Status: "PLANNING"}
	kitsu.search = []catalog.Manga{
		{Provider: catalog.KitsuProvider, ProviderID: "2"},
		{Provider: catalog.KitsuProvider, ProviderID: "3"},
	}
	if _, ok := svc.ProviderSuggestions(ctx, catalog.KitsuProvider); ok {
		t.Fatal("suggestions without a connected account")
	}
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	got, ok := svc.ProviderSuggestions(ctx, catalog.KitsuProvider)
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
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, nil); err != nil {
		t.Fatal(err)
	}
	if got := kitsu.savesCopy(); len(got) != 1 || got[0] != "9:2:COMPLETED:2024-03-01:2024-03-09" {
		t.Fatalf("saves = %v, want first-start and last-completion dates", got)
	}
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, nil); err != nil {
		t.Fatal(err)
	}
	if got := kitsu.savesCopy(); len(got) != 1 {
		t.Fatalf("dates re-pushed though remote has them: %v", got)
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
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, nil); err != nil {
		t.Fatal(err)
	}
	if links, _ := svc.TitleLinks(uctx, titleID); len(kitsu.savesCopy()) != 0 || links[0].Syncing {
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
	if err := svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, nil); err != nil {
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
	if _, recs, _ := svc.RelatedManga(uctx, nativeWork.ID, 10); len(recs) != 0 {
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
	if _, recs, _ := svc.RelatedManga(uctx, nativeWork.ID, 10); len(recs) != 1 {
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
func (r *recommendingProvider) Related(ctx context.Context, id string, limit int) ([]catalog.Manga, []catalog.Manga, error) {
	r.related++
	return r.fakeProvider.Related(ctx, id, limit)
}
func (r *recommendingProvider) Recommendations(context.Context, string, int) ([]catalog.Manga, error) {
	return []catalog.Manga{{Provider: catalog.MangaUpdatesProvider, ProviderID: "8", TitleRomaji: "Stub", IsAdult: true}}, nil
}

func TestSuggestionsShareOneListDownloadAndUseCheapRecommendations(t *testing.T) {
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
	got, ok := svc.ProviderSuggestions(ctx, catalog.MangaUpdatesProvider)
	if !ok || len(got) != 1 || got[0].TitleRomaji != "Known" || got[0].IsAdult {
		t.Fatalf("suggestions = %+v, want the stub filled from the stored record", got)
	}
	if mu.lists.Load() != 1 || mu.related != 0 {
		t.Fatalf("list downloads = %d, Related calls = %d; want 1 and 0", mu.lists.Load(), mu.related)
	}
	svc.invalidateRecs(1)
	svc.TrackerSuggestions(ctx, catalog.MangaUpdatesProvider)
	waitFor(t, func() bool { return mu.lists.Load() == 2 }, "a sync must refresh the cached list in the background")
	if err := svc.DisconnectTracker(ctx, 1, catalog.MangaUpdatesProvider); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.ProviderSuggestions(ctx, catalog.MangaUpdatesProvider); ok {
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
		c.fetched = time.Now().Add(-time.Minute)
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

func TestFavouriteSyncKeepsRemovalsFromEitherSide(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	kitsu := &favProvider{fakeProvider: newFake(catalog.KitsuProvider), favs: map[string]bool{"9": true}}
	svc.catalog.providers[catalog.KitsuProvider] = kitsu
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.KitsuProvider, ProviderID: "9", TitleRomaji: "Fav"}, 1, 0)
	local := func() bool {
		var n int
		_ = svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_favourites WHERE user_id = 1 AND title_id = ?`, titleID).Scan(&n)
		return n > 0
	}
	sync := func() {
		t.Helper()
		_ = svc.runTrackerSync(ctx, 1, catalog.KitsuProvider, nil)
	}

	sync()
	if !local() {
		t.Fatal("first sync should pull the remote favourite")
	}
	kitsu.fail = true
	if _, err := svc.lib.ToggleFavourite(ctx, titleID); err != nil {
		t.Fatal(err)
	}
	sync()
	kitsu.fail = false
	sync()
	if local() || kitsu.favs["9"] {
		t.Fatalf("local removal undone or not pushed: local=%v remote=%v", local(), kitsu.favs["9"])
	}
	kitsu.favs["9"] = true
	sync()
	if !local() {
		t.Fatal("a new remote favourite should be pulled")
	}
	kitsu.favs["9"] = false
	sync()
	if local() {
		t.Fatal("a remote removal should be pulled")
	}

	kitsu.favs["9"] = true
	sync()
	if err := svc.DisconnectTracker(ctx, 1, catalog.KitsuProvider); err != nil {
		t.Fatal(err)
	}
	connect(t, svc, ctx, catalog.KitsuProvider, time.Now())
	kitsu.favs["9"] = false
	sync()
	if !local() || !kitsu.favs["9"] {
		t.Fatalf("a reconnected account must start fresh, not drop the local favourite: local=%v remote=%v", local(), kitsu.favs["9"])
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
