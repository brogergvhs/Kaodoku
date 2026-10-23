package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/mangabaka"
)

const anthology = `{"id":34798,"state":"active","title":"Made in Abyss Official Anthology","type":"manga","status":"completed","content_rating":"safe","total_chapters":"40","cover":{"raw":{"url":"https://images.mangabaka.dev/a"}},"source":{"anilist":{"id":113956},"my_anime_list":{"id":130512},"kitsu":{"id":40211},"manga_updates":{"id":"wj13tgm"}}}
`

// useMangaBaka turns MangaBaka on with a copy imported from jsonl.
func useMangaBaka(t *testing.T, svc *JobService, ctx context.Context, jsonl string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "series.jsonl", Mode: 0o644, Size: int64(len(jsonl)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(jsonl))
	_ = tw.Close()
	_ = gz.Close()
	sum := sha1.Sum(buf.Bytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:])))
			return
		}
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	svc.mb = mangabaka.Open(filepath.Join(t.TempDir(), "mangabaka.db"))
	t.Cleanup(func() { _ = svc.mb.Close() })
	if _, err := svc.mb.Refresh(ctx, srv.Client(), srv.URL+"/series.jsonl.tar.gz", "full", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSetting(ctx, SettingMangaBakaEnabled, "true"); err != nil {
		t.Fatal(err)
	}
}

func TestMangaBakaLinksOneToOneAndSuggestsClashes(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	useMangaBaka(t, svc, ctx, anthology)
	titleID, work := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "113956", TitleRomaji: "Made in Abyss Anthology"}, 1, 0)
	_ = svc.catalog.repo.SetExternalID(ctx, work.ID, catalog.ExternalID{Provider: catalog.KitsuProvider, ProviderID: "999", Method: "cross_link", Confirmed: true})
	_ = svc.catalog.repo.SetExternalID(ctx, work.ID, catalog.ExternalID{Provider: catalog.MALProvider, Method: "user"})

	if err := svc.linkLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	maps := svc.mappings(ctx, work)
	if e := maps[catalog.MangaUpdatesProvider]; e.ProviderID != "70807749862" || e.Method != mangaBakaName || !e.Confirmed {
		t.Fatalf("mangaupdates = %+v, want a confirmed MangaBaka link", e)
	}
	if maps[catalog.KitsuProvider].ProviderID != "999" || maps[catalog.MALProvider].Method != "user" {
		t.Fatalf("existing links changed: %+v", maps)
	}
	links, _, _ := svc.MangaBakaLinks(ctx, catalog.MangaUpdatesProvider)
	_, kitsu, _ := svc.MangaBakaLinks(ctx, catalog.KitsuProvider)
	_, mal, _ := svc.MangaBakaLinks(ctx, catalog.MALProvider)
	if len(links) != 1 || len(kitsu) != 1 || kitsu[0].ID != "40211" || kitsu[0].Current != "999" || len(mal) != 1 || mal[0].Current != "" {
		t.Fatalf("links %+v, kitsu %+v, mal %+v", links, kitsu, mal)
	}

	if err := svc.AnswerMangaBakaSuggestion(ctx, titleID, catalog.MALProvider, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.linkLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	if _, mal, _ = svc.MangaBakaLinks(ctx, catalog.MALProvider); len(mal) != 0 {
		t.Fatalf("a dismissed suggestion came back: %+v", mal)
	}
	if err := svc.AnswerMangaBakaSuggestion(ctx, titleID, catalog.KitsuProvider, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmMangaBakaLink(ctx, titleID, catalog.MangaUpdatesProvider); err != nil {
		t.Fatal(err)
	}
	maps = svc.mappings(ctx, work)
	if maps[catalog.KitsuProvider].ProviderID != "40211" || maps[catalog.KitsuProvider].Method != "user" || maps[catalog.MangaUpdatesProvider].Method != "user" {
		t.Fatalf("after answering: %+v", maps)
	}
}

func TestMangaBakaSkipsIdsLinkedToAnotherTitle(t *testing.T) {
	svc, ctx := openTestJobs(t)
	useMangaBaka(t, svc, ctx, anthology)
	_, other := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1"}, 1, 0)
	_ = svc.catalog.repo.SetExternalID(ctx, other.ID, catalog.ExternalID{Provider: catalog.KitsuProvider, ProviderID: "40211", Method: "user", Confirmed: true})
	_, work := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "113956"}, 1, 0)
	svc.mbLink(ctx, []catalog.Manga{work})
	if e := svc.mappings(ctx, work)[catalog.KitsuProvider]; e.ProviderID != "" {
		t.Fatalf("kitsu = %+v, want nothing since another title holds 40211", e)
	}
}

func TestAggressiveResolveMakesNoTrackerCalls(t *testing.T) {
	svc, ctx := openTestJobs(t)
	useMangaBaka(t, svc, ctx, anthology)
	mu := &spyProvider{fakeProvider: newFake(catalog.MangaUpdatesProvider)}
	svc.catalog.providers[catalog.MangaUpdatesProvider] = mu
	_, known := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "113956"}, 1, 0)
	_, unknown := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "5", TitleRomaji: "Not in the dump"}, 1, 0)
	if id := svc.resolveID(ctx, known, mu); id != "70807749862" {
		t.Fatalf("resolved %q from MangaBaka", id)
	}
	if id := svc.resolveID(ctx, unknown, mu); id != "" || mu.calls.Load() != 0 {
		t.Fatalf("resolved %q with %d tracker calls, want none in aggressive mode", id, mu.calls.Load())
	}
}

// spyProvider counts calls that would reach a tracker's API for shared data.
type spyProvider struct {
	*fakeProvider
	calls atomic.Int32
}

func (p *spyProvider) Get(ctx context.Context, id string) (catalog.Manga, []catalog.Relation, error) {
	p.calls.Add(1)
	return p.fakeProvider.Get(ctx, id)
}
func (p *spyProvider) Search(ctx context.Context, q string, n int, f catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	p.calls.Add(1)
	return p.fakeProvider.Search(ctx, q, n, f)
}
func (p *spyProvider) Related(ctx context.Context, id string, n int) ([]catalog.Manga, []catalog.Manga, error) {
	p.calls.Add(1)
	return p.fakeProvider.Related(ctx, id, n)
}
func (p *spyProvider) Trending(ctx context.Context, n int) ([]catalog.Manga, error) {
	p.calls.Add(1)
	return p.fakeProvider.Trending(ctx, n)
}
