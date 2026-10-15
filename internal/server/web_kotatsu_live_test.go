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
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

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
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/health", nil))
	if !strings.Contains(rec.Body.String(), "Kotatsu worker") || !strings.Contains(rec.Body.String(), endpoint) {
		t.Fatalf("services health missing kotatsu worker: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "healthy") {
		t.Fatalf("kotatsu worker not reported healthy: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sources", nil))
	if body := rec.Body.String(); !strings.Contains(body, "Add Kotatsu source") || strings.Contains(body, "kotatsu-browse") {
		t.Fatalf("sources page should show only the add button, no browse block")
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu", nil))
	modal := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(modal, "kotatsu-add-modal") {
		t.Fatalf("modal: %d %s", rec.Code, modal)
	}
	m := regexp.MustCompile(`(\d+) parsers available`).FindStringSubmatch(modal)
	if m == nil {
		t.Fatalf("modal missing parser count: %s", modal)
	}
	if n, _ := strconv.Atoi(m[1]); n <= 0 || n >= 1370 {
		t.Fatalf("parser count %d not filtered (broken/nsfw should be excluded)", n)
	}
	if !strings.Contains(modal, `name="lang"`) || !strings.Contains(modal, `name="type"`) || !strings.Contains(modal, `name="status"`) {
		t.Fatalf("modal missing filters: %s", modal)
	}

	if strings.Contains(modal, "matching ·") {
		t.Fatalf("unfiltered modal should not show a matching count: %s", modal)
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu/list?q=mangadex&lang=en", nil))
	list := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(list, "MangaDex") {
		t.Fatalf("browse: %d %s", rec.Code, list)
	}
	if !strings.Contains(list, `type="checkbox"`) || !strings.Contains(list, `value="MANGADEX"`) {
		t.Fatalf("browse row missing selection checkbox: %s", list)
	}
	if strings.Count(list, `id="kotatsu-import-btn"`) != 1 {
		t.Fatalf("expected exactly one bulk import button: %s", list)
	}
	if !strings.Contains(list, "matching ·") {
		t.Fatalf("filtered list should show a matching count: %s", list)
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu/list?q=zzznosuchparser", nil))
	if !strings.Contains(rec.Body.String(), "No parser matches") {
		t.Fatalf("bogus query should match nothing: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu/list?type=comics", nil))
	c := regexp.MustCompile(`(\d+) matching · (\d+) parsers available`).FindStringSubmatch(rec.Body.String())
	if c == nil {
		t.Fatalf("type filter response missing counts: %s", rec.Body.String())
	}
	shown, _ := strconv.Atoi(c[1])
	total, _ := strconv.Atoi(c[2])
	if shown <= 0 || shown >= total {
		t.Fatalf("type filter did not narrow the list: %d of %d", shown, total)
	}

	form := url.Values{"id": {"MANGADEX", "AINZSCANS"}, "q": {"a"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/sources/kotatsu/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "imported") {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "matching ·") {
		t.Fatalf("import response should keep the active filter: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="kotatsu-sources-table" hx-swap-oob="true"`) {
		t.Fatalf("import response missing kotatsu table OOB swap: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verifying") {
		t.Fatalf("imported row not marked verifying: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="kotatsu-add-modal" hx-swap-oob="delete"`) {
		t.Fatalf("import response must close the modal: %s", rec.Body.String())
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
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu/list?q=mangadex&status=imported", nil))
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

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/sources/kotatsu:mangadex/edit", nil))
	edit := rec.Body.String()
	for _, f := range []string{"search_url", "extensions", "languages", "chapter_fetch", "image_fetch"} {
		if strings.Contains(edit, `name="`+f+`"`) {
			t.Fatalf("kotatsu edit dialog still offers %q: %s", f, edit)
		}
	}
	for _, f := range []string{"name", "base_url", "manga_url", "domains", "scraper", "nsfw", "enabled"} {
		if !strings.Contains(edit, `name="`+f+`"`) {
			t.Fatalf("kotatsu edit dialog missing %q", f)
		}
	}

	before, err := svc.GetSource(ctx, "kotatsu:mangadex")
	if err != nil {
		t.Fatal(err)
	}
	save := url.Values{
		"name": {before.Name}, "base_url": {before.BaseURL}, "manga_url": {before.SampleMangaURL},
		"domains": {strings.Join(before.Domains, ", ")}, "scraper": {before.Scraper}, "enabled": {"on"},
	}
	req = httptest.NewRequest(http.MethodPost, "/ui/sources/kotatsu:mangadex/edit", strings.NewReader(save.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit save: %d %s", rec.Code, rec.Body.String())
	}
	after, err := svc.GetSource(ctx, "kotatsu:mangadex")
	if err != nil {
		t.Fatal(err)
	}
	if after.Scraper != before.Scraper || after.SampleMangaURL != before.SampleMangaURL {
		t.Fatalf("edit save changed identity: %+v -> %+v", before, after)
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

	form = url.Values{"id": {"ONISAGA_EN"}}
	req = httptest.NewRequest(http.MethodPost, "/ui/sources/kotatsu/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import onisaga: %d %s", rec.Code, rec.Body.String())
	}
	health, err := kotatsu.New(endpoint, key).Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cf, cferr := svc.VerifySource(ctx, cfg, logSvc, "kotatsu:onisaga_en")
	if health.Solver {
		if cf.Status == sources.StatusRequiresCF {
			t.Fatalf("solver is enabled, cloudflare should be cleared: %q (%v)", cf.Status, cferr)
		}

		var chapters *sources.VerifyStep
		for i, step := range cf.Steps {
			if step.Name == "chapters" {
				chapters = &cf.Steps[i]
			}
		}
		if chapters == nil {
			t.Fatalf("verify reported no chapters step: %+v", cf.Steps)
		}
		if chapters.Status != "ok" {
			t.Fatalf("chapters step = %s (%s %s)", chapters.Status, chapters.Detail, chapters.Log)
		}
	} else {
		if cf.Status != sources.StatusRequiresCF {
			t.Fatalf("cloudflare source status = %q, want %q (err %v, steps %+v)", cf.Status, sources.StatusRequiresCF, cferr, cf.Steps)
		}
		if !strings.Contains(cf.Error, "kotatsu cloudflare_challenge:") {
			t.Fatalf("verify error should come from the typed worker error: %q", cf.Error)
		}
	}
}
