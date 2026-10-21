package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	chaptersPkg "github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/service"
)

func TestSettingValidation(t *testing.T) {
	for _, kv := range [][2]string{
		{service.SettingServeDownloadEvery, "15m"},
		{service.SettingJobsKeepHistory, "720h"},
		{service.SettingJobsKeepHistory, "0"},
		{service.SettingJobsKeepHistory, ""},
	} {
		if err := service.ValidateSetting(kv[0], kv[1]); err != nil {
			t.Errorf("%s=%q rejected: %v", kv[0], kv[1], err)
		}
	}
	for _, kv := range [][2]string{
		{service.SettingServeRunEvery, "0s"},
		{service.SettingBrowserSolverTimeoutSeconds, "0"},
		{service.SettingBrowserDownloaderTimeoutSeconds, "0"},
		{service.SettingJobsWorkers, "0"},
		{service.SettingJobsKeepHistory, "-1h"},
		{service.SettingJobsKeepHistory, "abc"},
	} {
		if service.ValidateSetting(kv[0], kv[1]) == nil {
			t.Errorf("%s=%q accepted", kv[0], kv[1])
		}
	}
}

func TestSettingsPageRedactsAniListSecret(t *testing.T) {
	ctx := context.Background()
	svc, closeDB, err := service.OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	api := New(
		svc,
		func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		},
	)
	if err := svc.SetSetting(ctx, service.SettingAniListClientSecret, "top-secret"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/server", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "top-secret") || !strings.Contains(body, redactedSecret) {
		t.Fatalf("settings secret redaction failed: %s", body)
	}

	form := url.Values{service.SettingAniListClientSecret: {redactedSecret}}
	req := httptest.NewRequest(http.MethodPut, "/ui/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings save status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := svc.Setting(ctx, service.SettingAniListClientSecret, ""); got != "top-secret" {
		t.Fatalf("stored secret = %q, want original", got)
	}
}

func TestSettingsToggleSelectAndMultiSave(t *testing.T) {
	ctx := context.Background()
	svc, closeDB, err := service.OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	api := New(svc, func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		})
	save := func(form url.Values) {
		req := httptest.NewRequest(http.MethodPut, "/ui/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "alert-error") {
			t.Fatalf("save %v: %d %s", form, rec.Code, rec.Body.String())
		}
	}
	save(url.Values{
		service.SettingRateLimitDisabled: {"false", "true"},
		service.SettingProvidersDisabled: {"", "kitsu", "mal"},
		service.SettingProvidersDefault:  {"mangaupdates"},
	})
	for key, want := range map[string]string{service.SettingRateLimitDisabled: "true", service.SettingProvidersDisabled: "kitsu,mal", service.SettingProvidersDefault: "mangaupdates"} {
		if got := svc.Setting(ctx, key, ""); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/server", nil))
	body := rec.Body.String()
	for _, want := range []string{`class="toggle" id="set-ratelimit.disabled" name="ratelimit.disabled" value="true" checked`, `value="kitsu" checked`, `data-empty-label="None">2 selected</summary>`, `<option value="mangaupdates" selected>MangaUpdates</option>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page missing %s", want)
		}
	}
	save(url.Values{service.SettingRateLimitDisabled: {"false"}, service.SettingProvidersDisabled: {""}})
	if svc.Setting(ctx, service.SettingRateLimitDisabled, "false") != "false" || svc.Setting(ctx, service.SettingProvidersDisabled, "") != "" {
		t.Fatal("unchecking did not clear the settings")
	}
}

func TestAPIReaderProgress(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "kaodoku.db")
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := library.NewRepository(db)
	title, err := repo.AddTitle(ctx, library.AddTitleParams{
		SourceURL:    "https://example.test/manga",
		DisplayTitle: "Example",
		Monitored:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertChapters(ctx, title.ID, []chaptersPkg.Chapter{{
		Chapter: providers.Chapter{URL: "https://example.test/ch-1", Label: "1", NumMain: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	chapter, err := repo.GetChapterByLabel(ctx, title.ID, "1")
	if err != nil {
		t.Fatal(err)
	}
	chapterFile := filepath.Join(t.TempDir(), "chapter-1.cbz")
	writeReaderTestCBZ(t, chapterFile)
	if err := repo.MarkDownloadCompleted(ctx, chapter.ID, chapterFile, 100, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	svc, closeDB, err := service.OpenJobs(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	api := New(
		svc,
		func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		},
	)

	var progress library.TitleReadProgress
	requestJSON(t, api, http.MethodGet, "/api/reader/titles/"+strconv.FormatInt(title.ID, 10), nil, http.StatusOK, &progress)
	if progress.NextChapterID != chapter.ID || progress.NextPage != 1 {
		t.Fatalf("reader progress = %+v, want chapter %d page 1", progress, chapter.ID)
	}

	var manifest readerManifestResponse
	requestJSON(t, api, http.MethodGet, "/api/reader/titles/"+strconv.FormatInt(title.ID, 10)+"/manifest", nil, http.StatusOK, &manifest)
	if len(manifest.Chapters) != 1 || len(manifest.Chapters[0].Pages) != 3 || manifest.Chapters[0].Pages[1].URL == "" {
		t.Fatalf("manifest = %+v, want one chapter with 3 page URLs", manifest)
	}

	req := httptest.NewRequest(http.MethodGet, "/reader/"+strconv.FormatInt(title.ID, 10), nil)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-reader`) || !strings.Contains(rec.Body.String(), manifest.Chapters[0].Pages[1].URL) {
		t.Fatalf("reader page status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/reader/chapters/"+strconv.FormatInt(chapter.ID, 10)+"/pages/2", nil)
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "two" {
		t.Fatalf("page 2 body = %q, want natural-order entry two", got)
	}

	var status library.ChapterReadStatus
	requestJSON(t, api, http.MethodPost, "/api/reader/chapters/"+strconv.FormatInt(chapter.ID, 10)+"/pages", map[string]int{"page": 1, "total_pages": 3}, http.StatusOK, &status)
	if status.ReadPages != 1 || status.FirstUnreadPage != 2 || status.Completed {
		t.Fatalf("page status = %+v, want page 2 incomplete", status)
	}
	requestJSON(t, api, http.MethodPost, "/api/reader/chapters/"+strconv.FormatInt(chapter.ID, 10)+"/complete", nil, http.StatusOK, &status)
	if !status.Completed || status.FirstUnreadPage != 0 {
		t.Fatalf("complete status = %+v, want complete", status)
	}
	requestJSON(t, api, http.MethodPost, "/api/reader/chapters/"+strconv.FormatInt(chapter.ID, 10)+"/unread", nil, http.StatusOK, &status)
	if status.Completed || status.ReadPages != 0 || status.FirstUnreadPage != 1 {
		t.Fatalf("unread status = %+v, want first page unread", status)
	}
	var rangeResult map[string]int
	requestJSON(t, api, http.MethodPost, "/api/reader/titles/"+strconv.FormatInt(title.ID, 10)+"/read-range", map[string]any{"from": "1", "to": "1", "read": true}, http.StatusOK, &rangeResult)
	if rangeResult["chapters"] != 1 {
		t.Fatalf("read range result = %+v, want 1", rangeResult)
	}
}

func writeReaderTestCBZ(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, item := range []struct {
		name string
		body string
	}{
		{"10.jpg", "ten"},
		{"1.jpg", "one"},
		{"2.jpg", "two"},
	} {
		w, err := zw.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, item.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSessionAuth(t *testing.T) {
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	api, closeDB := testAPI(t)
	defer closeDB()

	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}

	form := strings.NewReader("username=boss&password=secret123")
	login := httptest.NewRequest(http.MethodPost, "/login", form)
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, login)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kaodoku_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie issued")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func testAPI(t *testing.T) (http.Handler, func()) {

	t.Helper()
	svc, closeDB, err := service.OpenJobs(context.Background(), filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	return New(
		svc,
		func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		},
	), closeDB
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, body any, wantStatus int, out any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	if out != nil {
		if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMeAndServerPagesSplitSettings(t *testing.T) {
	ctx := context.Background()
	svc, closeDB, err := service.OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	api := New(svc, func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		})
	get := func(path string) string {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Body.String()
	}
	me, server := get("/me"), get("/server")
	if !strings.Contains(me, "/ui/account") || !strings.Contains(me, `name="ui.theme"`) || strings.Contains(me, "Scheduling") {
		t.Fatalf("/me should hold account and appearance only:\n%s", me)
	}
	if !strings.Contains(server, `id="app-anilist"`) || !strings.Contains(server, "getElementById('app-mal').showModal()") || !strings.Contains(server, "/trackers/mal/callback") {
		t.Fatal("/server is missing the tracker app rows and modals")
	}
	if strings.Contains(server, "/ui/account") || strings.Contains(server, `name="ui.theme"`) || !strings.Contains(server, "Scheduling") {
		t.Fatalf("/server should hold server settings only:\n%s", server)
	}
	if !strings.Contains(me, `href="/me"`) || !strings.Contains(me, "Log out") || !strings.Contains(me, `href="/server"`) {
		t.Fatal("account menu or Server link missing")
	}
	allow := func(on bool) bool {
		form := url.Values{"blocked_providers": {"kitsu"}}
		if on {
			form.Set("allow_adult", "on")
		}
		req := httptest.NewRequest(http.MethodPost, "/ui/account/content", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), "Saved") {
			t.Fatalf("content save: %s", rec.Body.String())
		}
		return strings.Contains(get("/me"), `name="allow_adult" checked`)
	}
	if !strings.Contains(me, "Content preferences") || !allow(true) || allow(false) || !strings.Contains(get("/me"), `value="kitsu" checked`) {
		t.Fatal("content preferences do not round-trip on /me")
	}
}

func TestSearchSwapsTagPickerWithBackend(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "kaodoku.db")
	svc, closeDB, err := service.OpenJobs(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := catalog.NewRepository(db)
	if err := repo.ReplaceContentTags(ctx, catalog.MALProvider, []catalog.ContentTag{{Name: "Gourmet", Kind: "genre"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceContentTags(ctx, catalog.AniListProvider, []catalog.ContentTag{{Name: "Isekai", Kind: "tag"}}); err != nil {
		t.Fatal(err)
	}
	api := New(svc, func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		})
	search := func(form url.Values) string {
		req := httptest.NewRequest(http.MethodPost, "/ui/search", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	swapped := search(url.Values{"provider": {"mal"}, "tags_for": {"anilist"}, "include_tags": {"Isekai"}})
	if !strings.Contains(swapped, `id="search-tags" class="filter-panel grid grid-cols-1 items-start gap-2 sm:grid-cols-2" hx-swap-oob="true"`) ||
		!strings.Contains(swapped, `data-tag-name="Gourmet"`) || strings.Contains(swapped, "Isekai") || !strings.Contains(swapped, `name="tags_for" value="mal"`) {
		t.Fatalf("backend switch should swap in MAL's tags and drop AniList picks:\n%s", swapped)
	}
	if same := search(url.Values{"provider": {"mal"}, "tags_for": {"mal"}}); strings.Contains(same, "hx-swap-oob") {
		t.Fatal("tag picker re-sent without a backend change")
	}
}
