package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
)

func TestMALXMLRoundTrip(t *testing.T) {
	svc, ctx := openTestJobs(t)
	ctx = asUser(svc, ctx, t)
	titleID, _ := seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "1", TitleRomaji: "Linked", ExternalIDs: map[string]string{catalog.MALProvider: "10"}}, 8, 3)
	seedTitle(t, svc, ctx, catalog.Manga{Provider: catalog.AniListProvider, ProviderID: "2", TitleRomaji: "Unlinked"}, 4, 1)
	if err := svc.storeRating(ctx, 1, titleID, 7); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	exported, skipped, err := svc.ExportMALXML(ctx, &out)
	xml := out.String()
	if err != nil || exported != 1 || skipped != 1 || !strings.Contains(xml, "<manga_mangadb_id>10</manga_mangadb_id>") ||
		!strings.Contains(xml, "<my_read_chapters>3</my_read_chapters>") || !strings.Contains(xml, "<my_score>7</my_score>") || !strings.Contains(xml, "<my_status>Reading</my_status>") {
		t.Fatalf("export %d/%d, %v:\n%s", exported, skipped, err, xml)
	}
	if _, err := svc.db.ExecContext(ctx, `DELETE FROM chapter_read_progress; DELETE FROM user_ratings`); err != nil {
		t.Fatal(err)
	}
	opts := ListImport{Progress: true, Ratings: true}
	if res, err := svc.ImportMALXML(ctx, strings.NewReader(xml), opts, false); err != nil || res.InLibrary != 1 || res.Raised != 0 {
		t.Fatalf("preview = %+v, %v", res, err)
	}
	res, err := svc.ImportMALXML(ctx, strings.NewReader(xml), opts, true)
	progress, _, _ := svc.localTrackerState(ctx, 1, titleID)
	rating, _ := svc.userRating(ctx, 1, titleID)
	if err != nil || res.Raised != 1 || res.Rated != 1 || progress != 3 || rating != 7 {
		t.Fatalf("import = %+v, %v; progress %d, rating %d", res, err, progress, rating)
	}
	if _, err := svc.ImportMALXML(ctx, strings.NewReader(strings.Replace(xml, "<user_export_type>2", "<user_export_type>1", 1)), opts, true); err == nil {
		t.Fatal("an anime export should be rejected")
	}
}
