package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

// Run with KAODOKU_KOTATSU_LIVE="<endpoint>|<key>" against a running worker.
func TestKotatsuBrowseImportLive(t *testing.T) {
	live := os.Getenv("KAODOKU_KOTATSU_LIVE")
	if live == "" {
		t.Skip("KAODOKU_KOTATSU_LIVE not set")
	}
	endpoint, key, _ := strings.Cut(live, "|")

	ctx := context.Background()
	svc, closeDB, err := service.OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	svc.SetRuntimeConfig(func() (*config.Config, ui.Log, error) {
		cfg := config.DefaultConfig()
		cfg.KotatsuWorker = config.KotatsuWorkerConfig{Enabled: true, Endpoint: endpoint, APIKey: key}
		return cfg, ui.New(ui.Options{Writer: io.Discard}), nil
	})
	api := New(
		svc,
		func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		},
	)

	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources", nil))
	m := regexp.MustCompile(`(\d+) parsers`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("sources page missing parser count badge")
	}
	if n, _ := strconv.Atoi(m[1]); n <= 0 || n >= 1370 {
		t.Fatalf("parser count %d not filtered (broken/nsfw should be excluded)", n)
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu?q=mangadex", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "MangaDex") {
		t.Fatalf("browse: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Import") {
		t.Fatalf("browse missing import button: %s", rec.Body.String())
	}

	form := url.Values{"id": {"MANGADEX"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/sources/kotatsu/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "imported") {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="kotatsu-sources-table" hx-swap-oob="true"`) {
		t.Fatalf("import response missing kotatsu table OOB swap: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verifying") {
		t.Fatalf("imported row not marked verifying: %s", rec.Body.String())
	}

	srcs, err := svc.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range srcs {
		if s.ID == "kotatsu:mangadex" {
			found = s.Scraper == "kotatsu:MANGADEX" && s.Enabled
			if s.SampleMangaURL == "" || s.SampleMangaURL == s.BaseURL {
				t.Fatalf("sample manga url not resolved: %q", s.SampleMangaURL)
			}
		}
	}
	if !found {
		t.Fatalf("imported source missing or wrong: %#v", srcs)
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu?q=mangadex", nil))
	if !strings.Contains(rec.Body.String(), "imported") {
		t.Fatalf("rebrowse missing imported badge: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources", nil))
	body := rec.Body.String()
	mainIdx := strings.Index(body, `id="sources-table"`)
	kotIdx := strings.Index(body, `id="kotatsu-sources-table"`)
	rowIdx := strings.Index(body, `id="src-kotatsu:mangadex"`)
	if mainIdx < 0 || kotIdx < 0 || rowIdx < 0 || !(mainIdx < kotIdx && kotIdx < rowIdx) {
		t.Fatalf("kotatsu source not in separate section below main table (main=%d kot=%d row=%d)", mainIdx, kotIdx, rowIdx)
	}

	// A pending sweep must show spinners without arming a poller per row.
	if _, err := svc.EnqueueSourceVerification(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources", nil))
	if polls := strings.Count(rec.Body.String(), `hx-trigger="load delay:1500ms"`); polls != 0 {
		t.Fatalf("queued sweep armed %d row pollers; want 0", polls)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/ui/sources/kotatsu:mangadex/verify", nil)
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "verifying") {
		t.Fatalf("verify kotatsu source: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `hx-trigger="load delay:1500ms"`) {
		t.Fatalf("clicked verify must arm its own poller: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu:mangadex/row", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "verifying") {
		t.Fatalf("row poll should stay verifying while job queued: %d %s", rec.Code, rec.Body.String())
	}

	cfg, logSvc, err := svc.RuntimeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DownloadDir = t.TempDir()
	res, verr := svc.VerifySource(ctx, cfg, logSvc, "kotatsu:mangadex")
	for _, step := range res.Steps {
		switch step.Name {
		case "search", "pick manga", "chapters":
			if step.Status != "ok" {
				t.Fatalf("verify step %s = %s (%s %s)", step.Name, step.Status, step.Detail, step.Log)
			}
		case "images":
			if step.Status != "ok" {
				t.Logf("images step %s (upstream flake tolerated): %s %v", step.Status, step.Log, verr)
			}
		}
	}
}
