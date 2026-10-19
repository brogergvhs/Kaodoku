package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	chaptersPkg "github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/service"
)

func do(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-API-Key", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPIV1AuthFlow(t *testing.T) {
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	api, closeDB := testAPI(t)
	defer closeDB()

	// meta is public and reports auth is on.
	var meta metaDTO
	rec := do(t, api, http.MethodGet, "/api/v1/meta", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta status = %d", rec.Code)
	}
	_ = json.NewDecoder(rec.Body).Decode(&meta)
	if !meta.AuthRequired || meta.APIVersion != 1 {
		t.Fatalf("meta = %+v", meta)
	}

	// wrong password -> 401.
	if rec := do(t, api, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "boss", "password": "nope"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d", rec.Code)
	}

	// login mints a token and returns me.
	rec = do(t, api, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "boss", "password": "secret123", "device_name": "test"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var login struct {
		Token string `json:"token"`
		Me    meDTO  `json:"me"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&login)
	if login.Token == "" || login.Me.User.Username != "boss" || len(login.Me.Permissions) == 0 {
		t.Fatalf("login body = %+v", login)
	}

	// me requires the token.
	if rec := do(t, api, http.MethodGet, "/api/v1/me", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me without token = %d", rec.Code)
	}
	if rec := do(t, api, http.MethodGet, "/api/v1/me", login.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("me with token = %d; body=%s", rec.Code, rec.Body.String())
	}

	// revoke, then the token no longer authenticates.
	if rec := do(t, api, http.MethodDelete, "/api/v1/auth/token", login.Token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", rec.Code)
	}
	if rec := do(t, api, http.MethodGet, "/api/v1/me", login.Token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after revoke = %d, want 401", rec.Code)
	}
}

func TestAPIV1MetaSingleUser(t *testing.T) {
	api, closeDB := testAPI(t) // no KAODOKU_ADMIN_PASSWORD -> single-user
	defer closeDB()
	var meta metaDTO
	rec := do(t, api, http.MethodGet, "/api/v1/meta", "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&meta)
	if meta.AuthRequired {
		t.Fatalf("single-user meta.auth_required = true")
	}
	if rec := do(t, api, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "x", "password": "y"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("single-user login status = %d, want 400", rec.Code)
	}
}

func TestAPIV1ReaderAndLibrary(t *testing.T) {
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
	title, err := repo.AddTitle(ctx, library.AddTitleParams{SourceURL: "https://example.test/m", DisplayTitle: "Example", Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertChapters(ctx, title.ID, []chaptersPkg.Chapter{{Chapter: providers.Chapter{URL: "https://example.test/ch-1", Label: "1", NumMain: 1}}}); err != nil {
		t.Fatal(err)
	}
	chapter, err := repo.GetChapterByLabel(ctx, title.ID, "1")
	if err != nil {
		t.Fatal(err)
	}
	cbz := filepath.Join(t.TempDir(), "c1.cbz")
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
	tid := strconv.FormatInt(title.ID, 10)
	cid := strconv.FormatInt(chapter.ID, 10)

	var list struct {
		Items      []titleDTO `json:"items"`
		Total      int        `json:"total"`
		NextCursor string     `json:"next_cursor"`
	}
	rec := do(t, api, http.MethodGet, "/api/v1/library", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("library status = %d", rec.Code)
	}
	_ = json.NewDecoder(rec.Body).Decode(&list)
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].DisplayTitle != "Example" {
		t.Fatalf("library = %+v", list)
	}
	if list.Items[0].CoverImage != fmt.Sprintf("/api/v1/covers/%d", title.ID) {
		t.Fatalf("cover_image = %q", list.Items[0].CoverImage)
	}

	var m readerManifestResponse
	rec = do(t, api, http.MethodGet, "/api/v1/reader/titles/"+tid+"/manifest", "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&m)
	if m.MarkBase != "/api/v1/reader/chapters/" {
		t.Fatalf("mark_base = %q", m.MarkBase)
	}
	if len(m.Chapters) == 0 || len(m.Chapters[0].Pages) == 0 || !strings.HasPrefix(m.Chapters[0].Pages[0].URL, "/api/v1/reader/") {
		t.Fatalf("manifest chapters = %+v", m.Chapters)
	}

	var cp chapterProgressDTO
	rec = do(t, api, http.MethodPost, "/api/v1/reader/chapters/"+cid+"/pages", "", map[string]int{"page": 1, "total_pages": 3})
	if rec.Code != http.StatusOK {
		t.Fatalf("mark page status = %d; body=%s", rec.Code, rec.Body.String())
	}
	_ = json.NewDecoder(rec.Body).Decode(&cp)
	if cp.ReadPages != 1 || cp.TotalPages != 3 {
		t.Fatalf("chapter progress = %+v", cp)
	}

	if rec := do(t, api, http.MethodGet, "/api/v1/reader/chapters/"+cid+"/pages/1", "", nil); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("page image status = %d len = %d", rec.Code, rec.Body.Len())
	}
	if rec := do(t, api, http.MethodGet, "/api/v1/library/999999", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing title status = %d", rec.Code)
	}

	rec = do(t, api, http.MethodGet, "/api/v1/reader/chapters/"+cid+"/archive", "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/vnd.comicbook+zip" || rec.Body.Len() == 0 {
		t.Fatalf("archive = %d %q len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	if rec := do(t, api, http.MethodGet, "/api/v1/reader/chapters/999999/archive", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing archive = %d", rec.Code)
	}

	past := "2026-01-02T10:00:00Z"
	rec = do(t, api, http.MethodPost, "/api/v1/reader/progress/batch", "", map[string]any{
		"entries": []map[string]any{
			{"chapter_id": chapter.ID, "page": 2, "total_pages": 3, "read_at": past},
			{"chapter_id": 999999, "page": 1, "total_pages": 3},
		},
	})
	var batch struct {
		Applied  int            `json:"applied"`
		Received int            `json:"received"`
		Skipped  map[string]int `json:"skipped"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&batch)
	if rec.Code != http.StatusOK || batch.Applied != 1 {
		t.Fatalf("batch = %d applied=%d", rec.Code, batch.Applied)
	}
	// A partial batch has to say why, or a client cannot tell a silent
	// no-op from a success and drops marks the server never stored.
	if batch.Received != 2 || batch.Skipped["unknown_chapter"] != 1 {
		t.Fatalf("batch receipt = %+v", batch)
	}
	rec = do(t, api, http.MethodPost, "/api/v1/reader/progress/batch", "", map[string]any{
		"entries": []map[string]any{
			{"page": 1, "total_pages": 3},
			{"chapter_id": chapter.ID, "page": 0, "total_pages": 3},
		},
	})
	batch.Skipped = nil
	_ = json.NewDecoder(rec.Body).Decode(&batch)
	if batch.Skipped["no_id"] != 1 || batch.Skipped["write_failed"] != 1 {
		t.Fatalf("skip reasons = %+v", batch.Skipped)
	}

	// A device clock ahead of the server's is clamped, not dropped.
	ahead := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = do(t, api, http.MethodPost, "/api/v1/reader/progress/batch", "", map[string]any{
		"entries": []map[string]any{
			{"chapter_id": chapter.ID, "page": 2, "total_pages": 3, "read_at": ahead},
		},
	})
	batch.Applied = 0
	_ = json.NewDecoder(rec.Body).Decode(&batch)
	if rec.Code != http.StatusOK || batch.Applied != 1 {
		t.Fatalf("future batch = %d applied=%d", rec.Code, batch.Applied)
	}

	var delta struct {
		Chapters   []chapterProgressDTO `json:"chapters"`
		ChapterIDs []int64              `json:"chapter_ids"`
		ServerTime string               `json:"server_time"`
	}
	rec = do(t, api, http.MethodGet, "/api/v1/reader/progress?since=2020-01-01T00:00:00Z", "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&delta)
	if rec.Code != http.StatusOK || len(delta.Chapters) != 1 || delta.Chapters[0].ReadPages != 2 || delta.ServerTime == "" {
		t.Fatalf("progress delta = %d %+v", rec.Code, delta)
	}
	if len(delta.ChapterIDs) != 1 || delta.ChapterIDs[0] != chapter.ID {
		t.Fatalf("chapter_ids = %v", delta.ChapterIDs)
	}
	rec = do(t, api, http.MethodGet, "/api/v1/reader/progress?since=2999-01-01T00:00:00Z", "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&delta)
	if len(delta.Chapters) != 0 {
		t.Fatalf("future-since delta = %+v", delta.Chapters)
	}

	var lib struct {
		Items []titleDTO `json:"items"`
		IDs   []int64    `json:"ids"`
	}
	rec = do(t, api, http.MethodGet, "/api/v1/library?since=2020-01-01T00:00:00Z", "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&lib)
	if len(lib.Items) != 1 || len(lib.IDs) != 1 || lib.IDs[0] != title.ID {
		t.Fatalf("library delta = %+v ids=%v", lib.Items, lib.IDs)
	}
	rec = do(t, api, http.MethodGet, "/api/v1/library?since=2999-01-01T00:00:00Z", "", nil)
	lib.Items, lib.IDs = nil, nil
	_ = json.NewDecoder(rec.Body).Decode(&lib)
	if len(lib.Items) != 0 || len(lib.IDs) != 1 {
		t.Fatalf("future-since library = items %d ids %d", len(lib.Items), len(lib.IDs))
	}
}

func seedTitle(t *testing.T, dbPath, url, name string, ownerID int64) int64 {
	t.Helper()
	ctx := auth.WithUser(context.Background(), &auth.User{ID: ownerID})
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	title, err := library.NewRepository(db).AddTitle(ctx, library.AddTitleParams{SourceURL: url, DisplayTitle: name, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	return title.ID
}

func TestAPIV1Actions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kaodoku.db")
	titleID := seedTitle(t, dbPath, "https://example.test/m", "Example", 1)
	svc, closeDB, err := service.OpenJobs(context.Background(), dbPath)
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
	tid := strconv.FormatInt(titleID, 10)

	if rec := do(t, api, http.MethodPut, "/api/v1/library/"+tid+"/favourite", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("favourite status = %d; %s", rec.Code, rec.Body.String())
	}
	var tdto titleDTO
	rec := do(t, api, http.MethodGet, "/api/v1/library/"+tid, "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&tdto)
	if !tdto.Favourite {
		t.Fatal("favourite not set")
	}
	rec = do(t, api, http.MethodPatch, "/api/v1/library/"+tid, "", map[string]any{"monitored": false})
	_ = json.NewDecoder(rec.Body).Decode(&tdto)
	if rec.Code != http.StatusOK || tdto.Monitored {
		t.Fatalf("patch monitored = %d %+v", rec.Code, tdto.Monitored)
	}

	var col collectionDTO
	rec = do(t, api, http.MethodPost, "/api/v1/collections", "", map[string]string{"name": "Faves"})
	_ = json.NewDecoder(rec.Body).Decode(&col)
	if rec.Code != http.StatusCreated || col.ID == 0 {
		t.Fatalf("collection create = %d %+v", rec.Code, col)
	}
	cid := strconv.FormatInt(col.ID, 10)
	if rec := do(t, api, http.MethodPut, "/api/v1/collections/"+cid+"/titles/"+tid, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("add member = %d", rec.Code)
	}
	rec = do(t, api, http.MethodGet, "/api/v1/collections/"+cid, "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&col)
	if len(col.TitleIDs) != 1 || col.TitleIDs[0] != titleID {
		t.Fatalf("members = %+v", col.TitleIDs)
	}

	var scr screenDTO
	rec = do(t, api, http.MethodPost, "/api/v1/screens", "", map[string]any{"name": "S1", "config": map[string]any{"sort": "title"}})
	_ = json.NewDecoder(rec.Body).Decode(&scr)
	if rec.Code != http.StatusOK || scr.ID == 0 || scr.Config.Sort != "title" {
		t.Fatalf("screen save = %d %+v", rec.Code, scr)
	}

	var us userSettingsDTO
	rec = do(t, api, http.MethodPut, "/api/v1/me/settings", "", map[string]any{"reader_mode": "paged", "theme": "nord", "reader_zoom": 1.5})
	_ = json.NewDecoder(rec.Body).Decode(&us)
	if rec.Code != http.StatusOK || us.ReaderMode != "paged" || us.Theme != "nord" || us.ReaderZoom == nil || *us.ReaderZoom != 1.5 {
		t.Fatalf("settings = %d %+v", rec.Code, us)
	}
	if rec := do(t, api, http.MethodPut, "/api/v1/me/settings", "", map[string]string{"theme": "bogus"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus theme status = %d", rec.Code)
	}

	rec = do(t, api, http.MethodGet, "/api/v1/sources", "", nil)
	var srcs struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&srcs)
	if rec.Code != http.StatusOK || len(srcs.Items) == 0 {
		t.Fatalf("sources = %d %+v", rec.Code, srcs)
	}
	if _, leaked := srcs.Items[0]["base_url"]; leaked || len(srcs.Items[0]) != 3 {
		t.Fatalf("source picker leaks fields: %+v", srcs.Items[0])
	}

}

func TestAPIV1EnqueueOwnership(t *testing.T) {
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	dbPath := filepath.Join(t.TempDir(), "kaodoku.db")
	svc, closeDB, err := service.OpenJobs(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	ctx := context.Background()
	roles, err := svc.Auth().ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var memberRole int64
	for _, ro := range roles {
		if ro.Name == "member" {
			memberRole = ro.ID
		}
	}
	if err := svc.Auth().CreateUser(ctx, "reader", "pass12345", memberRole, auth.ContentGuards{}); err != nil {
		t.Fatal(err)
	}
	users, _ := svc.Auth().ListUsers(ctx)
	var memberID int64
	for _, u := range users {
		if u.Username == "reader" {
			memberID = u.ID
		}
	}
	adminTitle := seedTitle(t, dbPath, "https://x.test/a", "AdminTitle", 1)
	memberTitle := seedTitle(t, dbPath, "https://x.test/b", "MemberTitle", memberID)

	api := New(svc,
		func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		},
	)
	login := func(user, pass string) string {
		rec := do(t, api, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": user, "password": pass})
		if rec.Code != http.StatusOK {
			t.Fatalf("login %s = %d", user, rec.Code)
		}
		var out struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&out)
		return out.Token
	}
	adminTok, memberTok := login("boss", "secret123"), login("reader", "pass12345")

	enqueue := func(token string, titleID int64, typ string) int {
		rec := do(t, api, http.MethodPost, "/api/v1/jobs/enqueue", token, map[string]any{"type": typ, "title_id": titleID})
		return rec.Code
	}
	if code := enqueue(memberTok, memberTitle, "refresh_title"); code != http.StatusCreated {
		t.Fatalf("member own title enqueue = %d, want 201", code)
	}
	if code := enqueue(memberTok, adminTitle, "refresh_title"); code != http.StatusCreated {
		t.Fatalf("member foreign title enqueue = %d, want 201 (web parity)", code)
	}
	if code := enqueue(memberTok, 0, "scan_downloads"); code != http.StatusForbidden {
		t.Fatalf("member global enqueue = %d, want 403", code)
	}
	if code := enqueue(adminTok, adminTitle, "refresh_title"); code != http.StatusCreated {
		t.Fatalf("admin enqueue = %d, want 201", code)
	}
	if rec := do(t, api, http.MethodPost, "/api/v1/jobs/run", memberTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("member jobs/run = %d, want 403", rec.Code)
	}
}

// Every v1 route except meta/login must reject unauthenticated requests.
// Keep in sync with registerAPIV1.
func TestAPIV1RequiresAuth(t *testing.T) {
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	api, closeDB := testAPI(t)
	defer closeDB()

	routes := []string{
		"GET /api/v1/me", "DELETE /api/v1/auth/token",
		"GET /api/v1/library", "GET /api/v1/library/1", "GET /api/v1/covers/1", "GET /api/v1/volumes/1/cover",
		"GET /api/v1/reader/titles/1", "GET /api/v1/reader/titles/1/manifest",
		"GET /api/v1/reader/chapters/1/pages/1", "GET /api/v1/reader/volumes/1/pages/1",
		"POST /api/v1/reader/chapters/1/pages", "POST /api/v1/reader/chapters/1/complete",
		"POST /api/v1/reader/chapters/1/unread", "POST /api/v1/reader/titles/1/read-range",
		"POST /api/v1/reader/volumes/1/pages", "GET /api/v1/reader/chapters/1/archive",
		"GET /api/v1/reader/volumes/1/archive", "GET /api/v1/reader/progress",
		"POST /api/v1/reader/progress/batch", "POST /api/v1/reader/volumes/1/read",
		"POST /api/v1/reader/volumes/1/unread", "POST /api/v1/reader/titles/1/volumes/read-range",
		"PUT /api/v1/library/1/favourite", "DELETE /api/v1/library/1/favourite",
		"PATCH /api/v1/library/1", "DELETE /api/v1/library/1",
		"GET /api/v1/collections", "POST /api/v1/collections", "GET /api/v1/collections/1",
		"PATCH /api/v1/collections/1", "DELETE /api/v1/collections/1",
		"PUT /api/v1/collections/1/titles/1", "DELETE /api/v1/collections/1/titles/1",
		"PUT /api/v1/collections/smart/k/pins/1", "DELETE /api/v1/collections/smart/k/pins/1",
		"GET /api/v1/screens", "POST /api/v1/screens", "PATCH /api/v1/screens/1",
		"DELETE /api/v1/screens/1", "POST /api/v1/screens/reorder",
		"GET /api/v1/me/settings", "PUT /api/v1/me/settings",
		"GET /api/v1/anilist", "POST /api/v1/anilist/sync", "DELETE /api/v1/anilist",
		"GET /api/v1/wanted/search", "GET /api/v1/wanted/trending", "GET /api/v1/wanted",
		"POST /api/v1/wanted", "GET /api/v1/wanted/matches", "POST /api/v1/wanted/matches",
		"POST /api/v1/wanted/track",
		"GET /api/v1/jobs", "GET /api/v1/jobs/1", "POST /api/v1/jobs/enqueue", "POST /api/v1/jobs/run",
		"GET /api/v1/sources",
	}
	for _, route := range routes {
		parts := strings.SplitN(route, " ", 2)
		rec := do(t, api, parts[0], parts[1], "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", route, rec.Code)
		}
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&body)
		if body.Code != "unauthorized" {
			t.Errorf("%s code = %q, want unauthorized", route, body.Code)
		}
	}
	if rec := do(t, api, http.MethodGet, "/api/v1/meta", "", nil); rec.Code != http.StatusOK {
		t.Errorf("meta = %d, want 200", rec.Code)
	}
}

func TestAPIV1LoginRateLimit(t *testing.T) {
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	api, closeDB := testAPI(t)
	defer closeDB()
	defer func() {
		loginRL.Lock()
		loginRL.hits = map[string]ipWindow{}
		loginRL.Unlock()
	}()
	last := 0
	for i := 0; i < 25; i++ {
		rec := do(t, api, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"username": "nobody", "password": "x"})
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("25th login = %d, want 429", last)
	}
}

func TestAPIV1MetaInstanceIDStable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kaodoku.db")
	metaID := func() string {
		svc, closeDB, err := service.OpenJobs(context.Background(), dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer closeDB()
		api := New(svc, func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
			func(context.Context, string) (service.SourceVerifyResult, error) {
				return service.SourceVerifyResult{}, nil
			})
		var meta metaDTO
		rec := do(t, api, http.MethodGet, "/api/v1/meta", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("meta status = %d", rec.Code)
		}
		_ = json.NewDecoder(rec.Body).Decode(&meta)
		return meta.InstanceID
	}
	first := metaID()
	if len(first) != 32 {
		t.Fatalf("instance_id = %q, want 32 hex chars", first)
	}
	if second := metaID(); second != first {
		t.Fatalf("instance_id changed across restart: %q -> %q", first, second)
	}
}

// A device re-login must replace that device's token, not accumulate them.
func TestAPIV1LoginReplacesDeviceToken(t *testing.T) {
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	api, closeDB := testAPI(t)
	defer closeDB()

	login := func(deviceID string) string {
		rec := do(t, api, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
			"username": "boss", "password": "secret123",
			"device_name": "iOS app · iPhone", "device_id": deviceID,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("login status = %d", rec.Code)
		}
		var out struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(rec.Body).Decode(&out)
		return out.Token
	}
	other := login("install-b") // same display name, different install
	first := login("install-a")
	second := login("install-a")
	// Same-install re-login replaces only that install's token.
	if rec := do(t, api, http.MethodGet, "/api/v1/me", first, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replaced install token still valid: %d", rec.Code)
	}
	for name, tok := range map[string]string{"other install": other, "new": second} {
		if rec := do(t, api, http.MethodGet, "/api/v1/me", tok, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s token rejected: %d", name, rec.Code)
		}
	}
}

func TestAPIV1LibraryAndJobsPaginateInSQL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kaodoku.db")
	var ids []int64
	for i := 1; i <= 5; i++ {
		ids = append(ids, seedTitle(t, dbPath, fmt.Sprintf("https://x.test/%d", i), fmt.Sprintf("T%d", i), 1))
	}
	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO catalog_manga(provider, provider_id, is_adult) VALUES ('anilist', 'adult', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE titles SET catalog_manga_id = (SELECT id FROM catalog_manga) WHERE id = ?`, ids[2]); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	svc, closeDB, err := service.OpenJobs(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	api := New(svc, nil, nil)

	type page struct {
		Items []titleDTO `json:"items"`
		Next  string     `json:"next_cursor"`
		Total int        `json:"total"`
	}
	get := func(path, token string) page {
		t.Helper()
		var p page
		rec := do(t, api, http.MethodGet, path, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		_ = json.NewDecoder(rec.Body).Decode(&p)
		return p
	}
	names := func(p page) string {
		var out []string
		for _, it := range p.Items {
			out = append(out, it.DisplayTitle)
		}
		return strings.Join(out, ",")
	}
	if p := get("/api/v1/library?limit=2&sort=title&dir=desc", ""); names(p) != "T5,T4" || p.Next != "2" || p.Total != 5 {
		t.Fatalf("first page = %s next=%q total=%d", names(p), p.Next, p.Total)
	}
	if p := get("/api/v1/library?limit=2&sort=title&dir=desc&cursor=4", ""); names(p) != "T1" || p.Next != "" {
		t.Fatalf("last page = %s next=%q", names(p), p.Next)
	}
	if p := get("/api/v1/library?cursor=10", ""); len(p.Items) != 0 || p.Total != 5 {
		t.Fatalf("past end = %+v", p)
	}
	if p := get("/api/v1/library?q=t3", ""); names(p) != "T3" {
		t.Fatalf("search = %s", names(p))
	}

	ctx := context.Background()
	roles, _ := svc.Auth().ListRoles(ctx)
	for _, ro := range roles {
		if ro.Name == "member" {
			if err := svc.Auth().CreateUser(ctx, "reader", "pass12345", ro.ID, auth.ContentGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	token, err := svc.Auth().CreateAPIToken(ctx, func() int64 {
		users, _ := svc.Auth().ListUsers(ctx)
		for _, u := range users {
			if u.Username == "reader" {
				return u.ID
			}
		}
		return 0
	}(), "test", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAODOKU_ADMIN_USER", "boss")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "secret123")
	if p := get("/api/v1/library?sort=title", token); p.Total != 4 || strings.Contains(names(p), "T3") {
		t.Fatalf("restricted library = %s total=%d", names(p), p.Total)
	}
	t.Setenv("KAODOKU_ADMIN_USER", "")
	t.Setenv("KAODOKU_ADMIN_PASSWORD", "")

	for _, id := range ids[:3] {
		if _, err := svc.Enqueue(ctx, "scan_downloads", id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var jl struct {
		Items []jobDTO `json:"items"`
		Next  string   `json:"next_cursor"`
	}
	rec := do(t, api, http.MethodGet, "/api/v1/jobs?limit=2", "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&jl)
	if len(jl.Items) != 2 || jl.Next == "" || jl.Items[0].ID < jl.Items[1].ID {
		t.Fatalf("jobs page 1 = %+v next=%q", jl.Items, jl.Next)
	}
	cursor := jl.Next
	jl.Items, jl.Next = nil, ""
	rec = do(t, api, http.MethodGet, "/api/v1/jobs?limit=2&status=queued&cursor="+cursor, "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&jl)
	if c, _ := strconv.ParseInt(cursor, 10, 64); len(jl.Items) != 1 || jl.Next != "" || jl.Items[0].ID >= c {
		t.Fatalf("jobs page 2 = %+v next=%q", jl.Items, jl.Next)
	}
	jl.Items = nil
	rec = do(t, api, http.MethodGet, "/api/v1/jobs?status=done", "", nil)
	_ = json.NewDecoder(rec.Body).Decode(&jl)
	if len(jl.Items) != 0 {
		t.Fatalf("done jobs = %+v", jl.Items)
	}
}
