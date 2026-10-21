package service

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
)

type vocabProvider struct {
	*fakeProvider
	tags   []catalog.ContentTag
	err    error
	filter catalog.SearchFilter
}

func (v *vocabProvider) TagVocabulary(context.Context) ([]catalog.ContentTag, error) {
	return v.tags, v.err
}
func (v *vocabProvider) Search(_ context.Context, _ string, _ int, f catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	v.filter = f
	return nil, false, nil
}

func TestProviderUnavailableUntilTagsSync(t *testing.T) {
	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	kitsu := &vocabProvider{fakeProvider: newFake(catalog.KitsuProvider), err: errors.New("down")}
	svc.catalog.providers[catalog.KitsuProvider] = kitsu

	if _, err := svc.Provider(ctx, catalog.KitsuProvider); err == nil || !strings.Contains(svc.ProviderOutage(catalog.KitsuProvider), "syncing") {
		t.Fatalf("unsynced provider usable: %v / %q", err, svc.ProviderOutage(catalog.KitsuProvider))
	}
	svc.SyncPendingVocabularies(ctx)
	if svc.ProviderOutage(catalog.KitsuProvider) == "" {
		t.Fatal("a failed sync must keep the provider unavailable")
	}

	kitsu.err = nil
	kitsu.tags = []catalog.ContentTag{{Name: "Horror", Kind: "genre", Slug: "horror"}, {Name: "Science Fiction", Kind: "genre", Slug: "science-fiction"}, {Name: "Nudity", Kind: "genre", IsAdult: true}}
	svc.SyncPendingVocabularies(ctx)
	if _, err := svc.Provider(ctx, catalog.KitsuProvider); err != nil {
		t.Fatalf("synced provider unavailable: %v", err)
	}
	if !slices.Contains(svc.AdultTagNames(ctx), "Nudity") {
		t.Fatal("provider adult tags must feed the adult guard")
	}

	if _, _, err := svc.SearchManga(ctx, catalog.KitsuProvider, "", 5, catalog.SearchFilter{GenreIn: []string{"science fiction"}, TagIn: []string{"Gore"}, GenreNotIn: []string{"Horror"}}); err != nil {
		t.Fatal(err)
	}
	if f := kitsu.filter; !slices.Equal(f.GenreIn, []string{"science-fiction"}) || len(f.TagIn) != 0 || !slices.Equal(f.GenreNotIn, []string{"horror"}) {
		t.Fatalf("filter sent to kitsu = %+v, want slugs and no unknown names", f)
	}
}
