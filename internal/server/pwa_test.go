package server

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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

func TestPWAManifest(t *testing.T) {
	api, closeDB := testAPI(t)
	defer closeDB()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	mani := get("/static/manifest.webmanifest")
	if mani.Code != http.StatusOK || !strings.Contains(mani.Body.String(), "\"standalone\"") {
		t.Fatalf("manifest status = %d body = %s", mani.Code, mani.Body.String())
	}
	if icon := get("/static/icon-192.png"); icon.Code != http.StatusOK {
		t.Errorf("icon status = %d", icon.Code)
	}
	if sw := get("/sw.js"); sw.Code == http.StatusOK {
		t.Error("/sw.js should no longer be served")
	}
}

func TestChapterDownloadToDevice(t *testing.T) {
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
	title, err := repo.AddTitle(ctx, library.AddTitleParams{SourceURL: "https://example.test/m", DisplayTitle: "Example"})
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
	cbz := filepath.Join(t.TempDir(), "chapter-1.cbz")
	writeReaderTestCBZ(t, cbz)
	if err := repo.MarkDownloadCompleted(ctx, chapter.ID, cbz, 100, 3); err != nil {
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
	api := New(svc,
		func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		},
	)

	do := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	one := do("/ui/library/" + strconv.FormatInt(title.ID, 10) + "/chapters/" + strconv.FormatInt(chapter.ID, 10) + "/download")
	if one.Code != http.StatusOK {
		t.Fatalf("chapter download status = %d", one.Code)
	}
	if cd := one.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".cbz") {
		t.Errorf("chapter Content-Disposition = %q", cd)
	}

	zipRec := do("/ui/library/" + strconv.FormatInt(title.ID, 10) + "/chapters/download?from=1&to=1")
	if zipRec.Code != http.StatusOK {
		t.Fatalf("zip download status = %d", zipRec.Code)
	}
	body := zipRec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip parse: %v", err)
	}
	if len(zr.File) != 1 || !strings.HasSuffix(zr.File[0].Name, ".cbz") {
		t.Fatalf("zip entries = %v", zr.File)
	}
	if cd := zipRec.Header().Get("Content-Disposition"); !strings.Contains(cd, "-1-1.zip") {
		t.Errorf("zip Content-Disposition = %q, want <title>-1-1.zip", cd)
	}

	if bad := do("/ui/library/" + strconv.FormatInt(title.ID, 10) + "/chapters/download"); bad.Code != http.StatusBadRequest {
		t.Errorf("missing from/to status = %d, want 400", bad.Code)
	}
	if none := do("/ui/library/" + strconv.FormatInt(title.ID, 10) + "/chapters/download?from=900&to=999"); none.Code != http.StatusNotFound {
		t.Errorf("empty-range status = %d, want 404", none.Code)
	}
}

func TestFetchCover(t *testing.T) {
	t.Parallel()
	_, svc, _, _ := opdsTestAPI(t)
	if _, _, ok := fetchCover(context.Background(), svc, "http://127.0.0.1/cover/large/x.jpg"); ok {
		t.Error("private cover URL should not fetch")
	}
	if _, _, ok := fetchCover(context.Background(), svc, ""); ok {
		t.Error("empty cover URL should not fetch")
	}
}
