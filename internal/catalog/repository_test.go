package catalog

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/database"
)

func TestRepositoryStoresWantedMangaAndMatches(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sources(id, name, base_url, sample_manga_url) VALUES ('demo', 'Demo', 'https://demo.test', 'https://demo.test/manga/sample')`); err != nil {
		t.Fatal(err)
	}

	repo := NewRepository(db)
	chapters := 12
	manga, err2 := repo.UpsertManga(ctx, Manga{Provider: "anilist", ProviderID: "9", TitleEnglish: "Frieren"})
	if err2 != nil {
		t.Fatal(err2)
	}
	_ = manga
	manga, _ = repo.UpsertManga(ctx, Manga{
		Provider:     AniListProvider,
		ProviderID:   "123",
		TitleEnglish: "Demo Manga",
		Chapters:     &chapters,
		Synonyms:     []string{"Demo", "Demo"},
		BannerImage:  "https://img.test/banner.jpg",
		Wanted:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !manga.Wanted || len(manga.Synonyms) != 1 || manga.BannerImage != "https://img.test/banner.jpg" {
		t.Fatalf("manga = %#v", manga)
	}

	match, err := repo.UpsertMatch(ctx, Match{
		CatalogMangaID: manga.ID,
		SourceID:       "demo",
		SourceURL:      "https://demo.test/manga/demo-manga",
		Confidence:     0.9,
		ChaptersFound:  12,
	})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := repo.ListMatches(ctx, manga.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].ID != match.ID || matches[0].ChaptersFound != 12 {
		t.Fatalf("matches = %#v", matches)
	}
}

func TestRecordMatchMissHiddenFromList(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sources(id, name, base_url, sample_manga_url) VALUES ('manhuaus', 'Manhuaus', 'https://manhuaus.com', 'https://manhuaus.com/manga/x')`); err != nil {
		t.Fatal(err)
	}

	repo := NewRepository(db)
	manga, err := repo.UpsertManga(ctx, Manga{Provider: "anilist", ProviderID: "9", TitleEnglish: "Frieren"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordMatchMiss(ctx, manga.ID, "manhuaus"); err != nil {
		t.Fatalf("RecordMatchMiss() error = %v", err)
	}

	all, err := repo.ListMatches(ctx, manga.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ChaptersFound != 0 || all[0].SourceID != "manhuaus" {
		t.Fatalf("ListMatches() = %#v, want one miss marker with 0 chapters", all)
	}
}
