package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	chaptersPkg "github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

func TestApplyLimitsFromSettings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()

	// Defaults before any override.
	if svc.jobs.MaxAttempts != 3 || svc.lib.repo.MaxDownloadAttempts != 3 || svc.jobTimeout != defaultJobTimeout || svc.jobWorkers != defaultJobWorkers {
		t.Fatalf("defaults = %d/%d/%s/%d", svc.jobs.MaxAttempts, svc.lib.repo.MaxDownloadAttempts, svc.jobTimeout, svc.jobWorkers)
	}

	for key, value := range map[string]string{
		SettingJobsMaxAttempts:      "5",
		SettingDownloadsMaxAttempts: "7",
		SettingJobsTimeout:          "2m",
		SettingJobsWorkers:          "6",
	} {
		if err := svc.SetSetting(ctx, key, value); err != nil {
			t.Fatalf("SetSetting(%s) error = %v", key, err)
		}
	}
	svc.applyLimits(ctx)

	if svc.jobs.MaxAttempts != 5 {
		t.Errorf("jobs.MaxAttempts = %d, want 5", svc.jobs.MaxAttempts)
	}
	if svc.lib.repo.MaxDownloadAttempts != 7 {
		t.Errorf("downloads MaxDownloadAttempts = %d, want 7", svc.lib.repo.MaxDownloadAttempts)
	}
	if svc.jobTimeout != 2*time.Minute {
		t.Errorf("jobTimeout = %s, want 2m", svc.jobTimeout)
	}
	if svc.jobWorkers != 6 {
		t.Errorf("jobWorkers = %d, want 6", svc.jobWorkers)
	}
}

func TestApplyBrowserDownloaderSettings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()

	for key, value := range map[string]string{
		SettingBrowserDownloaderEnabled:        "true",
		SettingBrowserDownloaderEndpoint:       "http://browser-worker:8192",
		SettingBrowserDownloaderTimeoutSeconds: "90",
	} {
		if err := svc.SetSetting(ctx, key, value); err != nil {
			t.Fatalf("SetSetting(%s) error = %v", key, err)
		}
	}

	var cfg config.Config
	svc.ApplySettings(ctx, &cfg)
	if !cfg.BrowserDownload.Enabled || cfg.BrowserDownload.Endpoint != "http://browser-worker:8192" || cfg.BrowserDownload.TimeoutSeconds != 90 {
		t.Fatalf("browser downloader config = %#v", cfg.BrowserDownload)
	}
}

func TestEnqueueTitleJobReusesActiveGlobalJob(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()

	global, err := svc.Enqueue(ctx, jobs.TypeDownloadMissing, 0, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(global) error = %v", err)
	}
	// A user's targeted download carries ResetFailed and must NOT be
	// swallowed by the plain global sweep job, or attempt-capped titles
	// could never be retried from the UI.
	targeted, err := svc.Enqueue(ctx, jobs.TypeDownloadMissing, 123, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(targeted) error = %v", err)
	}
	if targeted.ID == global.ID {
		t.Fatalf("reset request was swallowed by the global job %d", global.ID)
	}
	// But an identical second reset request is covered by the first.
	again, err := svc.Enqueue(ctx, jobs.TypeDownloadMissing, 123, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(again) error = %v", err)
	}
	if again.ID != targeted.ID {
		t.Fatalf("duplicate reset request = job %d, want %d", again.ID, targeted.ID)
	}
}

func TestEnqueueTitleJobReusesActiveTitleJob(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()

	pending, err := svc.enqueue(ctx, jobs.TypeDownloadMissing, JobPayload{TitleID: 123, ResetFailed: true}, time.Now())
	if err != nil {
		t.Fatalf("enqueue(pending) error = %v", err)
	}
	targeted, err := svc.Enqueue(ctx, jobs.TypeDownloadMissing, 123, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(targeted) error = %v", err)
	}
	if targeted.ID != pending.ID {
		t.Fatalf("targeted job ID = %d, want pending ID %d", targeted.ID, pending.ID)
	}
}

func TestGlobalDownloadMissingExpandsToMissingMonitoredTitles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	want := addJobTitle(t, ctx, svc, "https://example.test/want", true, 1, 0)
	empty := addJobTitle(t, ctx, svc, "https://example.test/empty", true, 0, 0)
	complete := addJobTitle(t, ctx, svc, "https://example.test/complete", true, 1, 1)
	off := addJobTitle(t, ctx, svc, "https://example.test/off", false, 1, 0)
	pending := addJobTitle(t, ctx, svc, "pending:demo", true, 0, 0)

	if err := svc.expandTitleJob(ctx, jobs.TypeDownloadMissing, 0); err != nil {
		t.Fatalf("expandTitleJob() error = %v", err)
	}
	assertTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, want.ID)
	assertTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, empty.ID)
	assertNoTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, complete.ID)
	assertNoTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, off.ID)
	assertNoTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, pending.ID)
}

func TestGlobalJobExpansionQueuesTitleJobsWhileGlobalIsRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	title := addJobTitle(t, ctx, svc, "https://example.test/want", true, 1, 0)
	global, err := svc.Enqueue(ctx, jobs.TypeDownloadMissing, 0, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(global) error = %v", err)
	}
	if _, err := svc.db.ExecContext(ctx, `UPDATE jobs SET status = 'running' WHERE id = ?`, global.ID); err != nil {
		t.Fatalf("mark global running: %v", err)
	}

	if err := svc.expandTitleJob(ctx, jobs.TypeDownloadMissing, 0); err != nil {
		t.Fatalf("expandTitleJob() error = %v", err)
	}
	assertTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, title.ID)
}

func TestLinkTitleSourceURLQueuesRefresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	addTestSource(t, ctx, svc, "demo")
	title, err := svc.lib.AddTitle(ctx, library.AddTitleParams{
		SourceURL:    "pending:demo",
		DisplayTitle: "Demo",
		Monitored:    true,
	})
	if err != nil {
		t.Fatalf("AddTitle() error = %v", err)
	}

	if _, err := svc.LinkTitleSourceURL(ctx, title.ID, "demo", "https://demo.test/manga/demo"); err != nil {
		t.Fatalf("LinkTitleSourceURL() error = %v", err)
	}
	assertTitleJob(t, ctx, svc, jobs.TypeRefreshTitle, title.ID)
	assertRefreshDownloadsAfter(t, ctx, svc, title.ID, true)
}

func TestSourceChangeQueuesRefreshForActiveTitles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	addTestSource(t, ctx, svc, "demo")
	title, err := svc.lib.AddTitle(ctx, library.AddTitleParams{
		SourceID:     "demo",
		SourceURL:    "https://demo.test/manga/demo",
		DisplayTitle: "Demo",
		Monitored:    true,
	})
	if err != nil {
		t.Fatalf("AddTitle() error = %v", err)
	}

	if err := svc.SetSourceMethods(ctx, "demo", sources.FetchHTTP, sources.FetchBrowser); err != nil {
		t.Fatalf("SetSourceMethods() error = %v", err)
	}
	assertTitleJob(t, ctx, svc, jobs.TypeRefreshTitle, title.ID)
	assertRefreshDownloadsAfter(t, ctx, svc, title.ID, true)
}

func TestTitleJobUpdatesSourceHealth(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	addTestSource(t, ctx, svc, "demo")
	title, err := svc.lib.AddTitle(ctx, library.AddTitleParams{
		SourceID:     "demo",
		SourceURL:    "https://demo.test/manga/demo",
		DisplayTitle: "Demo",
	})
	if err != nil {
		t.Fatalf("AddTitle() error = %v", err)
	}

	if err := svc.markTitleSourceHealth(ctx, title, nil, false); err != nil {
		t.Fatalf("mark healthy: %v", err)
	}
	src, err := svc.src.GetSource(ctx, "demo")
	if err != nil {
		t.Fatalf("Get(source) error = %v", err)
	}
	// A title update must not claim the source was verified.
	if src.Status != sources.StatusHealthy || src.LastError != "" || src.LastCheckedAt != "" {
		t.Fatalf("healthy source = %#v", src)
	}

	if err := svc.markTitleSourceHealth(ctx, title, errors.New("cloudflare blocked"), false); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	src, err = svc.src.GetSource(ctx, "demo")
	if err != nil {
		t.Fatalf("Get(source) error = %v", err)
	}
	// The error must name the title, since it did not come from a verify run.
	if src.Status != sources.StatusRequiresCF ||
		!strings.Contains(src.LastError, "cloudflare blocked") ||
		!strings.Contains(src.LastError, title.DisplayTitle) {
		t.Fatalf("failed source = %#v", src)
	}
}

func TestEnqueueSourceVerificationQueuesEnabledSources(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	addTestSource(t, ctx, svc, "on")
	addTestSource(t, ctx, svc, "off")
	if err := svc.SetSourceEnabled(ctx, "off", false); err != nil {
		t.Fatalf("SetSourceEnabled() error = %v", err)
	}

	n, err := svc.EnqueueSourceVerification(ctx, time.Now())
	if err != nil {
		t.Fatalf("EnqueueSourceVerification() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("queued = %d, want 1", n)
	}
	assertSourceJob(t, ctx, svc, "on")
	assertNoSourceJob(t, ctx, svc, "off")
}

func TestBackupUserDataJobCreatesBackup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	if _, err := svc.Enqueue(ctx, jobs.TypeBackupUserData, 0, time.Now()); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	summary, err := svc.RunDue(ctx, config.DefaultConfig(), ui.NewLogger(false))
	if err != nil {
		t.Fatalf("RunDue() error = %v", err)
	}
	if summary.Done != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	backups, err := svc.ListBackups(ctx)
	if err != nil {
		t.Fatalf("ListBackups() error = %v", err)
	}
	if len(backups) != 1 || backups[0].Size == 0 {
		t.Fatalf("backups = %#v", backups)
	}
}

func TestAutoRefreshQueuesDownloadWheneverChaptersAreMissing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	many := addJobTitle(t, ctx, svc, "https://example.test/many", true, 2, 0)
	one := addJobTitle(t, ctx, svc, "https://example.test/one", true, 1, 0)
	// Finished titles are tracked unmonitored — linking a source must still
	// download them.
	finished := addJobTitle(t, ctx, svc, "https://example.test/finished", false, 3, 0)
	complete := addJobTitle(t, ctx, svc, "https://example.test/complete", true, 2, 2)

	for _, title := range []library.Title{many, one, finished, complete} {
		if err := svc.enqueueDownloadAfterRefresh(ctx, title.ID); err != nil {
			t.Fatalf("enqueueDownloadAfterRefresh(%s) error = %v", title.SourceURL, err)
		}
	}
	assertTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, many.ID)
	assertTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, one.ID)
	assertTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, finished.ID)
	// Nothing missing: nothing to queue.
	assertNoTitleJob(t, ctx, svc, jobs.TypeDownloadMissing, complete.ID)
}

func TestRemoveTitleCancelsTitleJobs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()
	title := addJobTitle(t, ctx, svc, "https://example.test/remove", true, 1, 0)
	job, err := svc.enqueueExact(ctx, jobs.TypeDownloadMissing, JobPayload{TitleID: title.ID}, time.Now())
	if err != nil {
		t.Fatalf("enqueueExact() error = %v", err)
	}

	if _, err := svc.RemoveTitleFiles(ctx, title.ID, false, false); err != nil {
		t.Fatalf("RemoveTitle() error = %v", err)
	}
	job, err = svc.jobs.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get(job) error = %v", err)
	}
	if job.Status != "cancelled" {
		t.Fatalf("job status = %q, want cancelled", job.Status)
	}
}

func addTestSource(t *testing.T, ctx context.Context, svc *JobService, id string) {
	t.Helper()
	if err := svc.ImportLocalSource(ctx, sources.Profile{
		ID:                id,
		Name:              id,
		Domains:           []string{id + ".test"},
		BaseURL:           "https://" + id + ".test/",
		SampleMangaURL:    "https://" + id + ".test/manga/demo",
		Scraper:           "generic",
		AllowedExtensions: []string{"jpg"},
		Enabled:           true,
	}); err != nil {
		t.Fatalf("ImportLocalSource() error = %v", err)
	}
}

func assertTitleJob(t *testing.T, ctx context.Context, svc *JobService, typ string, titleID int64) {
	t.Helper()
	js, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if hasTitleJob(js, typ, titleID) {
		return
	}
	t.Fatalf("job %s for title %d not found in %#v", typ, titleID, js)
}

func assertNoTitleJob(t *testing.T, ctx context.Context, svc *JobService, typ string, titleID int64) {
	t.Helper()
	js, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if hasTitleJob(js, typ, titleID) {
		t.Fatalf("unexpected job %s for title %d in %#v", typ, titleID, js)
	}
}

func assertSourceJob(t *testing.T, ctx context.Context, svc *JobService, sourceID string) {
	t.Helper()
	js, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if hasSourceJob(js, sourceID) {
		return
	}
	t.Fatalf("source job for %s not found in %#v", sourceID, js)
}

func assertNoSourceJob(t *testing.T, ctx context.Context, svc *JobService, sourceID string) {
	t.Helper()
	js, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if hasSourceJob(js, sourceID) {
		t.Fatalf("unexpected source job for %s in %#v", sourceID, js)
	}
}

func assertRefreshDownloadsAfter(t *testing.T, ctx context.Context, svc *JobService, titleID int64, want bool) {
	t.Helper()
	js, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, job := range js {
		var payload JobPayload
		if job.Type == jobs.TypeRefreshTitle && json.Unmarshal([]byte(job.Payload), &payload) == nil && payload.TitleID == titleID {
			if payload.DownloadAfterRefresh != want {
				t.Fatalf("DownloadAfterRefresh = %t, want %t in %#v", payload.DownloadAfterRefresh, want, job)
			}
			return
		}
	}
	t.Fatalf("refresh job for title %d not found in %#v", titleID, js)
}

func hasSourceJob(js []jobs.Job, sourceID string) bool {
	for _, job := range js {
		var payload JobPayload
		if job.Type == jobs.TypeVerifySource && json.Unmarshal([]byte(job.Payload), &payload) == nil && payload.SourceID == sourceID {
			return true
		}
	}
	return false
}

func hasTitleJob(js []jobs.Job, typ string, titleID int64) bool {
	for _, job := range js {
		var payload JobPayload
		if job.Type == typ && json.Unmarshal([]byte(job.Payload), &payload) == nil && payload.TitleID == titleID {
			return true
		}
	}
	return false
}

func addJobTitle(t *testing.T, ctx context.Context, svc *JobService, sourceURL string, monitored bool, chapters, completed int) library.Title {
	t.Helper()
	title, err := svc.lib.AddTitle(ctx, library.AddTitleParams{
		SourceURL:    sourceURL,
		DisplayTitle: sourceURL,
		Monitored:    monitored,
	})
	if err != nil {
		t.Fatalf("AddTitle() error = %v", err)
	}
	for i := 1; i <= chapters; i++ {
		if _, err := svc.lib.repo.UpsertChapters(ctx, title.ID, []chaptersPkg.Chapter{{
			Chapter: providers.Chapter{
				URL:     sourceURL + "/chapter-" + strconv.Itoa(i),
				Label:   strconv.Itoa(i),
				NumMain: i,
			},
		}}); err != nil {
			t.Fatalf("UpsertChapters() error = %v", err)
		}
	}
	for i := 1; i <= completed; i++ {
		ch, err := svc.lib.repo.GetChapterByLabel(ctx, title.ID, strconv.Itoa(i))
		if err != nil {
			t.Fatalf("GetChapterByLabel() error = %v", err)
		}
		if err := svc.lib.repo.MarkDownloadCompleted(ctx, ch.ID, "/tmp/ch.cbz", 1, 1); err != nil {
			t.Fatalf("MarkDownloadCompleted() error = %v", err)
		}
	}
	title, err = svc.lib.GetTitle(ctx, title.ID)
	if err != nil {
		t.Fatalf("GetTitle() error = %v", err)
	}
	return title
}

func TestLastJobTime(t *testing.T) {
	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("OpenJobs() error = %v", err)
	}
	defer closeDB()

	if got := svc.LastJobTime(ctx, jobs.TypeBackupUserData); !got.IsZero() {
		t.Fatalf("LastJobTime(never ran) = %v, want zero", got)
	}
	if _, err := svc.Enqueue(ctx, jobs.TypeBackupUserData, 0, time.Now()); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	got := svc.LastJobTime(ctx, jobs.TypeBackupUserData)
	if got.IsZero() || time.Since(got) > time.Minute {
		t.Fatalf("LastJobTime(just enqueued) = %v, want recent", got)
	}
}
