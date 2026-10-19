package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers"
)

func TestTitleRefreshDue(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	cases := []struct {
		name     string
		interval string
		last     *time.Time
		want     bool
	}{
		{"empty follows global", "", ago(time.Minute), true},
		{"never refreshed", "6h", nil, true},
		{"not yet due", "6h", ago(2 * time.Hour), false},
		{"due", "6h", ago(7 * time.Hour), true},
		{"invalid falls through", "nonsense", ago(time.Minute), true},
	}
	for _, tc := range cases {
		got := titleRefreshDue(library.Title{RefreshInterval: tc.interval, LastRefreshedAt: tc.last}, now)
		if got != tc.want {
			t.Errorf("%s: due=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestExpandTitleJobSelectsTitlesInSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	repo := svc.lib.repo
	add := func(url string, monitored bool, chapterCount, downloaded int) int64 {
		t.Helper()
		title, err := repo.AddTitle(ctx, library.AddTitleParams{SourceURL: url, DisplayTitle: url, Monitored: monitored})
		if err != nil {
			t.Fatal(err)
		}
		var list []chapters.Chapter
		for i := 1; i <= chapterCount; i++ {
			list = append(list, chapters.Chapter{Chapter: providers.Chapter{URL: fmt.Sprintf("%s/%d", url, i), Label: fmt.Sprint(i), NumMain: i}})
		}
		if _, err := repo.UpsertChapters(ctx, title.ID, list); err != nil {
			t.Fatal(err)
		}
		chs, _ := repo.ListChapters(ctx, title.ID)
		for i := 0; i < downloaded; i++ {
			if err := repo.MarkDownloadCompleted(ctx, chs[i].ID, "/x.cbz", 10, 2); err != nil {
				t.Fatal(err)
			}
		}
		return title.ID
	}
	missing := add("https://x.test/missing", true, 2, 1)
	complete := add("https://x.test/complete", true, 2, 2)
	add("local:folder", true, 0, 0)
	add("pending:42", true, 0, 0)
	unmonitored := add("https://x.test/unmonitored", false, 2, 1)
	notDue := add("https://x.test/not-due", true, 0, 0)
	if _, err := svc.db.ExecContext(ctx, `UPDATE titles SET refresh_interval = '6h', last_refreshed_at = ? WHERE id = ?`, database.FormatTime(time.Now().Add(-time.Hour)), notDue); err != nil {
		t.Fatal(err)
	}

	for parent, tc := range []struct {
		typ  string
		want []int64
	}{
		{jobs.TypeRefreshTitle, []int64{missing, complete}},
		{jobs.TypeDownloadMissing, []int64{missing, notDue}},
		{jobs.TypeScanDownloads, []int64{missing, complete, unmonitored}},
	} {
		if err := svc.expandTitleJob(ctx, tc.typ, int64(parent+1000)); err != nil {
			t.Fatal(err)
		}
		rows, err := svc.db.QueryContext(ctx, `SELECT json_extract(payload_json, '$.title_id') FROM jobs WHERE parent_id = ? ORDER BY 1`, parent+1000)
		if err != nil {
			t.Fatal(err)
		}
		got := []int64{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		rows.Close()
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s children = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func TestLinkAlwaysAutoDownloadsAfterRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	fresh := library.Title{ID: 11, SourceURL: "https://example.test/fresh", DiscoveredCount: 0}
	existing := library.Title{ID: 12, SourceURL: "https://example.test/existing", DiscoveredCount: 40}
	if err := svc.enqueueRefreshForTitle(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := svc.enqueueRefreshForTitle(ctx, existing); err != nil {
		t.Fatal(err)
	}
	all, _ := svc.List(ctx)
	got := map[int64]bool{}
	for _, j := range all {
		var p JobPayload
		if json.Unmarshal([]byte(j.Payload), &p) == nil && j.Type == jobs.TypeRefreshTitle {
			got[p.TitleID] = p.DownloadAfterRefresh
		}
	}
	if !got[11] || !got[12] {
		t.Fatalf("linking must always download after refresh; payloads=%v", got)
	}
}
