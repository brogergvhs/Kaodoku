package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
)

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

func TestFilterTitlesBySourceID(t *testing.T) {
	t.Parallel()

	titles := []library.Title{
		{ID: 1, SourceID: "mangadex", SourceURL: "https://mangadex.org/x"},
		{ID: 2, SourceID: "other", SourceURL: "https://other.to/y"},
		{ID: 3, SourceURL: "local:import"},
	}
	got := filterTitles(append([]library.Title(nil), titles...), libraryControls{
		Sources: []string{"mangadex"}, SourceLinks: map[int64]bool{3: true},
		Monitor: "all", Fav: "all", Content: "all", Progress: "all",
	})
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("source filter kept %#v", got)
	}
	got = filterTitles(append([]library.Title(nil), titles...), libraryControls{
		Sources: []string{"linked"}, Monitor: "all", Fav: "all", Content: "all", Progress: "all",
	})
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("linked filter kept %#v", got)
	}
	got = filterTitles(append([]library.Title(nil), titles...), libraryControls{
		Monitor: "all", Fav: "all", Content: "all", Progress: "all",
	})
	if len(got) != 3 {
		t.Fatalf("all filter kept %#v", got)
	}
	got = filterTitles(append([]library.Title(nil), titles...), libraryControls{
		Sources: []string{"mangadex", "other"}, Monitor: "all", Fav: "all", Content: "all", Progress: "all",
	})
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("multi-source filter kept %#v", got)
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
