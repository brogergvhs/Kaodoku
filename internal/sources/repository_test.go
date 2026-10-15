package sources

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/database"
)

func TestRepositorySyncListAndUpdateCheck(t *testing.T) {
	t.Parallel()

	ctx, repo, closeDB := testRepository(t)
	defer closeDB()
	profile := Profile{
		ID:                      "Example",
		Name:                    "Example",
		Domains:                 []string{"example.test", "example.test"},
		BaseURL:                 "https://example.test/",
		SampleMangaURL:          "https://example.test/manga/demo/",
		SearchURL:               "https://example.test/search?q={query}",
		AllowedExtensions:       []string{"webp", "jpg"},
		MinChapters:             3,
		RequiresBrowserDownload: true,
		Enabled:                 true,
	}
	if err := repo.Sync(ctx, []Profile{profile}, OriginLocal); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if err := repo.UpdateCheck(ctx, "example", CheckResult{
		Status: StatusHealthy, ChaptersFound: 12, ImagesFound: 4,
		ImageExtensions: []string{"webp"}, ChapterFetch: FetchHTTP, ImageFetch: FetchBrowser,
		Steps: []VerifyStep{{Name: "chapters", Status: StepOK, Detail: "12 chapters"}},
	}); err != nil {
		t.Fatalf("UpdateCheck() error = %v", err)
	}

	got, err := repo.Get(ctx, "example")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ChapterFetch != FetchHTTP || got.ImageFetch != FetchBrowser {
		t.Fatalf("fetch methods = %q/%q, want http/browser", got.ChapterFetch, got.ImageFetch)
	}
	if got.Origin != OriginLocal || got.Status != StatusHealthy || got.ChaptersFound != 12 || got.SampleImagesFound != 4 {
		t.Fatalf("source status = %#v", got)
	}
	if len(got.Domains) != 1 || got.Domains[0] != "example.test" {
		t.Fatalf("domains = %#v", got.Domains)
	}
	if len(got.ImageExtensions) != 1 || got.ImageExtensions[0] != "webp" {
		t.Fatalf("image extensions = %#v", got.ImageExtensions)
	}
	if len(got.VerifySteps) != 1 || got.VerifySteps[0].Status != StepOK {
		t.Fatalf("verify steps = %#v", got.VerifySteps)
	}
	if !got.RequiresBrowserDownload {
		t.Fatalf("RequiresBrowserDownload = false")
	}
	if got.SearchURL != "https://example.test/search?q={query}" {
		t.Fatalf("SearchURL = %q", got.SearchURL)
	}

	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 1 || all[0].ID != "example" {
		t.Fatalf("List() = %#v", all)
	}
}

func TestRepositoryLocalOverridesSyncedSources(t *testing.T) {
	t.Parallel()

	ctx, repo, closeDB := testRepository(t)
	defer closeDB()

	local := Profile{
		ID:             "demo",
		Name:           "Local Demo",
		BaseURL:        "https://local.test/",
		SampleMangaURL: "https://local.test/manga/demo/",
		Enabled:        true,
	}
	builtin := Profile{
		ID:             "demo",
		Name:           "Built In Demo",
		BaseURL:        "https://builtin.test/",
		SampleMangaURL: "https://builtin.test/manga/demo/",
		Enabled:        true,
	}
	if err := repo.Sync(ctx, []Profile{local}, OriginLocal); err != nil {
		t.Fatal(err)
	}
	if err := repo.Sync(ctx, []Profile{builtin}, OriginBuiltin); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != OriginLocal || got.Name != "Local Demo" {
		t.Fatalf("local override was not preserved: %#v", got)
	}
	if err := repo.RemoveLocal(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, "demo"); err == nil {
		t.Fatalf("Get() after RemoveLocal() succeeded")
	}
}

func TestDecodeRegistry(t *testing.T) {
	t.Parallel()

	body := []byte(`
profiles:
  - id: demo
    name: Demo
    base_url: https://demo.test/
    sample_manga_url: https://demo.test/manga/sample/
    search_url: https://demo.test/search?q={query}
    allowed_extensions: [webp]
`)
	got, err := DecodeRegistry(body)
	if err != nil {
		t.Fatalf("DecodeRegistry() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "demo" || got[0].Scraper != "generic" {
		t.Fatalf("profiles = %#v", got)
	}
	if got[0].SearchURL == "" {
		t.Fatalf("SearchURL was not decoded: %#v", got[0])
	}
}

func TestBuiltInProfiles(t *testing.T) {
	t.Parallel()

	got, err := BuiltInProfiles()
	if err != nil {
		t.Fatalf("BuiltInProfiles() error = %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("BuiltInProfiles() returned no profiles")
	}
	for _, profile := range got {
		if profile.ID == "" || profile.BaseURL == "" || profile.SampleMangaURL == "" {
			t.Fatalf("incomplete profile = %#v", profile)
		}
		if profile.ID == "comickz" && profile.Scraper != "comickz" {
			t.Fatalf("comickz profile = %#v", profile)
		}
	}
}

func testRepository(t *testing.T) (context.Context, *Repository, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return ctx, NewRepository(db), func() { _ = db.Close() }
}

func TestTitleCounts(t *testing.T) {
	t.Parallel()

	ctx, repo, closeDB := testRepository(t)
	defer closeDB()
	if err := repo.Sync(ctx, []Profile{
		{ID: "alpha", Name: "Alpha", BaseURL: "https://a.test/", SampleMangaURL: "https://a.test/m/1", Enabled: true},
		{ID: "beta", Name: "Beta", BaseURL: "https://b.test/", SampleMangaURL: "https://b.test/m/1", Enabled: true},
	}, OriginLocal); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO titles (id, source_id, source_url, display_title) VALUES (1, 'alpha', 'https://a.test/m/1', 'One')`,
		`INSERT INTO titles (id, source_id, source_url, display_title) VALUES (2, 'alpha', 'https://a.test/m/2', 'Two')`,
		`INSERT INTO titles (id, source_id, source_url, display_title) VALUES (3, NULL, 'pending:t3', 'Three')`,
		// title 1 already counts for alpha via titles.source_id: must not double-count.
		`INSERT INTO title_sources (title_id, source_id, url) VALUES (1, 'alpha', 'https://a.test/m/1')`,
		`INSERT INTO title_sources (title_id, source_id, url) VALUES (1, 'beta', 'https://b.test/m/1')`,
	} {
		if _, err := repo.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.TitleCounts(ctx)
	if err != nil {
		t.Fatalf("TitleCounts() error = %v", err)
	}
	if got["alpha"] != 2 {
		t.Errorf("alpha count = %d, want 2", got["alpha"])
	}
	if got["beta"] != 1 {
		t.Errorf("beta count = %d, want 1", got["beta"])
	}
	if _, ok := got[""]; ok {
		t.Error("empty source id must not be counted")
	}
}
