package service

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
)

func TestAdultTagNamesFailsClosed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()

	// Populated vocabulary: adult names come from the store plus the seed;
	// non-adult stays visible.
	if err := svc.catalog.repo.ReplaceContentTags(ctx, catalog.MangaUpdatesProvider, []catalog.ContentTag{
		{Name: "Action", Kind: "genre"},
		{Name: "Adult", Kind: "genre"},
		{Name: "Guro", Kind: "tag", IsAdult: true},
		{Name: "Kids", Kind: "tag"},
	}); err != nil {
		t.Fatalf("ReplaceContentTags() error = %v", err)
	}
	names := svc.AdultTagNames(ctx)
	if !slices.Contains(names, "Guro") || !slices.Contains(names, "Adult") {
		t.Error("stored adult tag missing from AdultTagNames")
	}
	if !slices.Contains(names, "Hentai") || !slices.Contains(names, "Netorare") {
		t.Error("baked-in seed missing from AdultTagNames")
	}
	if slices.Contains(names, "Kids") || slices.Contains(names, "Action") {
		t.Error("non-adult tag/genre must not be reported adult")
	}
}
