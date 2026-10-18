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
	genres := []string{"Action", "Hentai"}
	tags := []ContentTag{
		{Name: "Kids", Kind: "tag"},
		{Name: "Bondage", Kind: "tag", IsAdult: true},
	}
	if err := repo.ReplaceContentTags(ctx, genres, tags); err != nil {
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
	if err := repo.ReplaceContentTags(ctx, []string{"Action"}, []ContentTag{
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
