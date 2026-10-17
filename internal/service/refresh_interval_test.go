package service

import (
	"context"
	"encoding/json"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"path/filepath"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/library"
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

func TestGlobalJobsSkipUnlinkedTitles(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://mangapill.com/manga/1/x", true},
		{"local:Infinite%20leveling%20murim", false},
		{"pending:42", false},
	} {
		title := library.Title{SourceURL: tc.url, Monitored: true, MissingCount: 3, DiscoveredCount: 10}
		if got := globalTitleJobApplies("refresh_title", title, now); got != tc.want {
			t.Errorf("refresh applies(%q) = %v, want %v", tc.url, got, tc.want)
		}
		if got := globalTitleJobApplies("download_missing", title, now); got != tc.want {
			t.Errorf("download applies(%q) = %v, want %v", tc.url, got, tc.want)
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
