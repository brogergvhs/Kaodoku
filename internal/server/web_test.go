package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
)

func TestTagFilterViewGroupsByCategory(t *testing.T) {
	t.Parallel()

	// Options arrive in the reader's order: genres first, then category asc.
	opts := []catalog.ContentTag{
		{Name: "Action", Kind: "genre"},
		{Name: "Comedy", Kind: "genre"},
		{Name: "Loose End", Kind: "tag"}, // empty category -> Other
		{Name: "Time Travel", Kind: "tag", Category: "Setting-Time"},
		{Name: "Magic", Kind: "tag", Category: "Theme-Fantasy", Description: "Spells and such"},
	}
	v := tagFilterView(opts, []string{"Magic"}, []string{"Action", "StaleCustom"}, "Tags")
	groups := v["Groups"].([]tagGroupView)
	titles := make([]string, len(groups))
	for i, g := range groups {
		titles[i] = g.Title
	}
	// Genres pinned first; category groups in input order; Custom last.
	want := []string{"Genres", "Other", "Setting – Time", "Theme – Fantasy", "Custom"}
	if strings.Join(titles, ",") != strings.Join(want, ",") {
		t.Fatalf("group order = %v, want %v", titles, want)
	}
	if v["IncludeCount"] != 1 || v["ExcludeCount"] != 2 {
		t.Fatalf("counts = inc %v exc %v", v["IncludeCount"], v["ExcludeCount"])
	}
	// Magic chip is included and carries its description tooltip.
	for _, g := range groups {
		for _, c := range g.Tags {
			if c.Name == "Magic" && (c.State != "include" || c.Tooltip != "Spells and such") {
				t.Fatalf("magic chip = %+v", c)
			}
			if c.Name == "StaleCustom" && c.State != "exclude" {
				t.Fatalf("custom chip state = %q", c.State)
			}
		}
	}
}

func TestTitleActivityFromTargetedTitleJob(t *testing.T) {
	t.Parallel()

	title := library.Title{ID: 42, Monitored: true}
	// A queued job locks the button, shows no live label, and is listed as queued.
	running, label, queued, failed, _ := titleActivityFrom([]jobs.Job{{
		Type:    jobs.TypeDownloadMissing,
		Status:  "queued",
		Payload: `{"title_id":42}`,
	}}, title)
	if failed || label != "" || !running[jobs.TypeDownloadMissing] || len(queued) != 1 || queued[0] != "downloading" {
		t.Fatalf("queued: running %v label %q queued %v failed %v", running, label, queued, failed)
	}
	// A running job surfaces the live label instead.
	running, label, queued, failed, _ = titleActivityFrom([]jobs.Job{{
		Type:    jobs.TypeDownloadMissing,
		Status:  "running",
		Payload: `{"title_id":42}`,
	}}, title)
	if failed || label != "downloading" || len(queued) != 0 || !running[jobs.TypeDownloadMissing] {
		t.Fatalf("running: running %v label %q queued %v failed %v", running, label, queued, failed)
	}
}

func TestTitleActivityFromIgnoresGlobalTitleJob(t *testing.T) {
	t.Parallel()

	running, label, queued, failed, _ := titleActivityFrom([]jobs.Job{{
		Type:    jobs.TypeDownloadMissing,
		Status:  "queued",
		Payload: `{}`,
	}}, library.Title{ID: 42, Monitored: true})
	if len(running) != 0 || label != "" || len(queued) != 0 || failed {
		t.Fatalf("activity = running %v label %q failed %v", running, label, failed)
	}
}

func TestSelectedTitleIDs(t *testing.T) {
	t.Parallel()

	got, err := selectedTitleIDs([]string{"2", "2", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Fatalf("ids = %#v", got)
	}
	if _, err := selectedTitleIDs(nil); err == nil {
		t.Fatal("empty selection accepted")
	}
	if _, err := selectedTitleIDs([]string{"x"}); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestLibraryPageSourceOptionsRender(t *testing.T) {
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))
	view := libraryView{
		Controls:      libraryControls{Sources: []string{"mangadex"}, Monitor: "all", Fav: "all", Content: "all", Progress: "all"},
		SourceOptions: []sources.Source{{Profile: sources.Profile{ID: "mangadex", Name: "MangaDex"}}},
	}
	var buf bytes.Buffer
	if err := u.tmpl.ExecuteTemplate(&buf, "library", view); err != nil {
		t.Fatalf("render library: %v", err)
	}
	if !strings.Contains(buf.String(), `name="source" value="mangadex" checked`) {
		t.Error("source option not rendered checked")
	}
}

func TestReaderManifestWindow(t *testing.T) {
	t.Parallel()

	progress := library.TitleReadProgress{
		Title: library.Title{ID: 7, DisplayTitle: "Demo"},
		Chapters: []library.ChapterReadStatus{
			{Chapter: library.Chapter{ID: 1, Label: "1"}, TotalPages: 2, FirstUnreadPage: 1},
			{Chapter: library.Chapter{ID: 2, Label: "2"}, TotalPages: 2, FirstUnreadPage: 2, ReadPages: 1},
			{Chapter: library.Chapter{ID: 3, Label: "3"}, TotalPages: 2, FirstUnreadPage: 1},
			{Chapter: library.Chapter{ID: 4, Label: "4"}, TotalPages: 2, FirstUnreadPage: 1},
		},
		NextChapterID: 2,
		NextPage:      2,
	}

	// Hard cutoff: the strip starts at the current chapter (no previous
	// chapters above it); prev remains reachable via navigation only.
	manifest, prevID, nextID := readerManifestWindowMode(progress, 0, false)
	if prevID != 1 || nextID != 3 {
		t.Fatalf("prev/next = %d/%d, want 1/3", prevID, nextID)
	}
	if len(manifest.Chapters) != 2 || manifest.Chapters[0].ID != 2 || manifest.Chapters[1].ID != 3 ||
		manifest.ResumeChapterID != 2 || manifest.ResumePage != 2 {
		t.Fatalf("manifest = %+v, want chapters [2 3] resuming at 2/2", manifest)
	}

	manifest, prevID, nextID = readerManifestWindowMode(progress, 4, false)
	if prevID != 3 || nextID != 0 || len(manifest.Chapters) != 1 || manifest.Chapters[0].ID != 4 {
		t.Fatalf("requested tail manifest = %+v prev=%d next=%d", manifest, prevID, nextID)
	}
}
