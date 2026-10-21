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
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
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
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
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
