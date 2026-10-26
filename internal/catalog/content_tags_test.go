package catalog

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/database"
)

func TestContentTagsAdultRoundTrip(t *testing.T) {
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

	repo := NewRepository(db)
	tags := []ContentTag{
		{Name: "Action", Kind: "genre"},
		{Name: "Hentai", Kind: "genre"},
		{Name: "Kids", Kind: "tag"},
		{Name: "Bondage", Kind: "tag", IsAdult: true},
	}
	if err := repo.ReplaceContentTags(ctx, AniListProvider, tags); err != nil {
		t.Fatalf("ReplaceContentTags() error = %v", err)
	}
	stored, err := repo.ContentTags(ctx)
	if err != nil {
		t.Fatalf("ContentTags() error = %v", err)
	}
	adult := map[string]bool{}
	for _, tag := range stored {
		adult[tag.Name] = tag.IsAdult
	}
	if len(stored) != 4 {
		t.Fatalf("stored %d tags, want 4", len(stored))
	}
	if !adult["Hentai"] || !adult["Bondage"] {
		t.Error("Hentai genre and Bondage tag should be adult")
	}
	if adult["Action"] || adult["Kids"] {
		t.Error("Action and Kids must not be adult")
	}

	if err := repo.ReplaceContentTags(ctx, KitsuProvider, []ContentTag{{Name: "action", Kind: "genre", Slug: "action"}, {Name: "Smut", Kind: "genre"}, {Name: "Space", Kind: "genre", Slug: "space"}}); err != nil {
		t.Fatal(err)
	}
	merged, _ := repo.ContentTags(ctx)
	kitsu, _ := repo.ProviderTags(ctx, KitsuProvider)
	if len(merged) != 6 || len(kitsu) != 3 || kitsu[2].Slug != "space" || !kitsu[1].IsAdult {
		t.Fatalf("merged=%+v kitsu=%+v", merged, kitsu)
	}
	if !repo.HasProviderTags(ctx, KitsuProvider) || repo.HasProviderTags(ctx, MALProvider) {
		t.Fatal("HasProviderTags wrong")
	}
	if err := repo.ReplaceContentTags(ctx, KitsuProvider, []ContentTag{{Name: "Space", Kind: "genre"}}); err != nil {
		t.Fatal(err)
	}
	if again, _ := repo.ContentTags(ctx); len(again) != 5 {
		t.Fatalf("replacing one provider touched another: %+v", again)
	}
}

func TestContentTagsCategoryRoundTrip(t *testing.T) {
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
	repo := NewRepository(db)
	if err := repo.ReplaceContentTags(ctx, AniListProvider, []ContentTag{
		{Name: "Action", Kind: "genre"},
		{Name: "Time Travel", Kind: "tag", Category: "Setting-Time", Description: "Moves through time"},
	}); err != nil {
		t.Fatalf("ReplaceContentTags() error = %v", err)
	}
	stored, err := repo.ContentTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var tt ContentTag
	for _, s := range stored {
		if s.Name == "Time Travel" {
			tt = s
		}
	}
	if tt.Category != "Setting-Time" || tt.Description != "Moves through time" {
		t.Fatalf("round-trip lost metadata: %+v", tt)
	}
}
