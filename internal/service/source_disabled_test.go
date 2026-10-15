package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
)

func TestSourceForTitleDisabledSourceStopsRefresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	js := newJobService(db)
	svc := js.lib
	repo := sources.NewRepository(db)
	profile := sources.Profile{
		ID: "kotatsu:testsrc", Name: "Test", BaseURL: "https://t.test/",
		SampleMangaURL: "https://t.test/m/1", Scraper: "kotatsu:TESTSRC", Enabled: true,
	}
	if err := repo.Sync(ctx, []sources.Profile{profile}, sources.OriginLocal); err != nil {
		t.Fatal(err)
	}
	title := library.Title{SourceID: "kotatsu:testsrc", SourceURL: "https://t.test/m/1"}

	src, ok, err := svc.sourceForTitle(ctx, title)
	if err != nil || !ok || src.ID != "kotatsu:testsrc" {
		t.Fatalf("enabled source: got (%q, %v, %v)", src.ID, ok, err)
	}

	if err := repo.SetEnabled(ctx, "kotatsu:testsrc", false); err != nil {
		t.Fatal(err)
	}
	_, ok, err = svc.sourceForTitle(ctx, title)
	if !errors.Is(err, errSourceDisabled) {
		t.Fatalf("disabled source: err = %v, want errSourceDisabled", err)
	}
	if ok {
		t.Fatal("disabled source must not resolve to any scraper")
	}

	wrapped := fmt.Errorf("refresh %q: %w", "Some Title", err)
	if err := js.markTitleSourceHealth(ctx, title, wrapped, false); err != nil {
		t.Fatalf("markTitleSourceHealth() error = %v", err)
	}
	got, err := repo.Get(ctx, "kotatsu:testsrc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "unknown" || got.LastError != "" {
		t.Fatalf("disabled source health written: status %q, last_error %q", got.Status, got.LastError)
	}
}
