package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/util"
)

const maxPageBytes = 64 << 20 // per-image download cap
const appTokenTTLDays = 90

type apiV1 struct {
	svc        *service.JobService
	runJobs    func(context.Context) (service.RunSummary, error)
	instanceID string
}

func loadInstanceID(svc *service.JobService) string {
	ctx := context.Background()
	if id := svc.Setting(ctx, "instance_id", ""); id != "" {
		return id
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	id := hex.EncodeToString(raw)
	if err := svc.SetSetting(ctx, "instance_id", id); err != nil {
		return ""
	}
	return id
}

// registerAPIV1 mounts the /api/v1 surface consumed by the native app.
func registerAPIV1(mux *http.ServeMux, svc *service.JobService, runJobs func(context.Context) (service.RunSummary, error)) {
	a := &apiV1{svc: svc, runJobs: runJobs, instanceID: loadInstanceID(svc)}
	mux.HandleFunc("GET /api/v1/meta", a.meta)
	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.HandleFunc("GET /api/v1/me", a.me)
	mux.HandleFunc("DELETE /api/v1/auth/token", a.revokeToken)

	mux.HandleFunc("GET /api/v1/library", a.libraryList)
	mux.HandleFunc("GET /api/v1/library/{id}", a.libraryGet)
	mux.HandleFunc("GET /api/v1/covers/{id}", a.titleCover)
	mux.HandleFunc("GET /api/v1/volumes/{id}/cover", func(w http.ResponseWriter, r *http.Request) { serveVolumeCover(w, r, svc) })

	mux.HandleFunc("GET /api/v1/reader/titles/{id}", a.readerTitle)
	mux.HandleFunc("GET /api/v1/reader/titles/{id}/manifest", a.manifest)
	mux.HandleFunc("GET /api/v1/reader/chapters/{id}/pages/{page}", func(w http.ResponseWriter, r *http.Request) { serveChapterPage(w, r, svc) })
	mux.HandleFunc("GET /api/v1/reader/volumes/{id}/pages/{page}", func(w http.ResponseWriter, r *http.Request) { serveVolumePage(w, r, svc) })
	mux.HandleFunc("POST /api/v1/reader/chapters/{id}/pages", a.markPage)
	mux.HandleFunc("POST /api/v1/reader/chapters/{id}/complete", a.markComplete)
	mux.HandleFunc("POST /api/v1/reader/chapters/{id}/unread", a.markUnread)
	mux.HandleFunc("POST /api/v1/reader/titles/{id}/read-range", a.readRange)
	mux.HandleFunc("POST /api/v1/reader/volumes/{id}/pages", a.markVolumePage)
	mux.HandleFunc("GET /api/v1/reader/chapters/{id}/archive", a.chapterArchive)
	mux.HandleFunc("GET /api/v1/reader/volumes/{id}/archive", a.volumeArchive)
	mux.HandleFunc("GET /api/v1/reader/progress", a.progressSince)
	mux.HandleFunc("POST /api/v1/reader/progress/batch", a.progressBatch)
	mux.HandleFunc("POST /api/v1/reader/volumes/{id}/read", a.volumeSetRead(true))
	mux.HandleFunc("POST /api/v1/reader/volumes/{id}/unread", a.volumeSetRead(false))
	mux.HandleFunc("POST /api/v1/reader/titles/{id}/volumes/read-range", a.volumesReadRange)

	mux.HandleFunc("PUT /api/v1/library/{id}/favourite", a.setFavourite(true))
	mux.HandleFunc("DELETE /api/v1/library/{id}/favourite", a.setFavourite(false))
	mux.HandleFunc("PATCH /api/v1/library/{id}", a.patchTitle)
	mux.HandleFunc("DELETE /api/v1/library/{id}", a.deleteTitle)

	mux.HandleFunc("GET /api/v1/collections", a.collectionsList)
	mux.HandleFunc("POST /api/v1/collections", a.collectionCreate)
	mux.HandleFunc("GET /api/v1/collections/{id}", a.collectionGet)
	mux.HandleFunc("PATCH /api/v1/collections/{id}", a.collectionPatch)
	mux.HandleFunc("DELETE /api/v1/collections/{id}", a.collectionDelete)
	mux.HandleFunc("PUT /api/v1/collections/{id}/titles/{titleId}", a.collectionMember(true))
	mux.HandleFunc("DELETE /api/v1/collections/{id}/titles/{titleId}", a.collectionMember(false))
	mux.HandleFunc("PUT /api/v1/collections/smart/{key}/pins/{titleId}", a.smartPin(true))
	mux.HandleFunc("DELETE /api/v1/collections/smart/{key}/pins/{titleId}", a.smartPin(false))

	mux.HandleFunc("GET /api/v1/screens", a.screensList)
	mux.HandleFunc("POST /api/v1/screens", a.screenSave)
	mux.HandleFunc("PATCH /api/v1/screens/{id}", a.screenSave)
	mux.HandleFunc("DELETE /api/v1/screens/{id}", a.screenDelete)
	mux.HandleFunc("POST /api/v1/screens/reorder", a.screensReorder)

	mux.HandleFunc("GET /api/v1/me/settings", a.meSettingsGet)
	mux.HandleFunc("PUT /api/v1/me/settings", a.meSettingsPut)
	mux.HandleFunc("GET /api/v1/anilist", a.anilistStatus)
	mux.HandleFunc("POST /api/v1/anilist/sync", a.anilistSync)
	mux.HandleFunc("DELETE /api/v1/anilist", a.anilistDisconnect)

	mux.HandleFunc("GET /api/v1/wanted/search", a.wantedSearch)
	mux.HandleFunc("GET /api/v1/tags", a.tagOptions)

	mux.HandleFunc("GET /api/v1/library/{id}/activity", a.titleActivity)
	mux.HandleFunc("GET /api/v1/library/{id}/sources", a.titleSources)
	mux.HandleFunc("POST /api/v1/library/{id}/sources/find", a.sourcesFind)
	mux.HandleFunc("POST /api/v1/library/{id}/sources/link", a.sourcesLink)
	mux.HandleFunc("POST /api/v1/library/{id}/sources/link-url", a.sourcesLinkURL)
	mux.HandleFunc("POST /api/v1/library/{id}/sources/unlink", a.sourcesUnlink)
	mux.HandleFunc("GET /api/v1/sources/manage", a.sourcesManage)
	mux.HandleFunc("POST /api/v1/sources/{id}/enabled", a.sourceSetEnabled)
	mux.HandleFunc("POST /api/v1/sources/{id}/verify", a.sourceVerify)
	mux.HandleFunc("POST /api/v1/library/add", a.libraryAdd)
	mux.HandleFunc("GET /api/v1/wanted/trending", a.wantedTrending)
	mux.HandleFunc("GET /api/v1/wanted", a.wantedList)
	mux.HandleFunc("POST /api/v1/wanted", a.wantedAdd)
	mux.HandleFunc("GET /api/v1/wanted/matches", a.matchesList)
	mux.HandleFunc("POST /api/v1/wanted/matches", a.matchesFind)
	mux.HandleFunc("POST /api/v1/wanted/track", a.track)

	mux.HandleFunc("GET /api/v1/jobs", a.jobsList)
	mux.HandleFunc("GET /api/v1/jobs/{id}", a.jobGet)
	mux.HandleFunc("POST /api/v1/jobs/enqueue", a.jobEnqueue)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", a.jobCancel)
	mux.HandleFunc("POST /api/v1/jobs/run", jobsRunV1(runJobs))
	mux.HandleFunc("GET /api/v1/sources", a.sourcesPick)
}

// v1err writes the {error, code} envelope shared by every v1 endpoint.
func v1err(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func serverVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

type metaDTO struct {
	ServerVersion string   `json:"server_version"`
	APIVersion    int      `json:"api_version"`
	AuthRequired  bool     `json:"auth_required"`
	Features      []string `json:"features"`
	ImageFormats  []string `json:"image_formats"`
	MaxPageBytes  int64    `json:"max_page_bytes"`
	InstanceID    string   `json:"instance_id,omitempty"`
}

type meDTO struct {
	User        meUserDTO  `json:"user"`
	Permissions []string   `json:"permissions"`
	AniList     aniListDTO `json:"anilist"`
}

type meUserDTO struct {
	ID         int64  `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	AllowAdult bool   `json:"allow_adult"`
}

type aniListDTO struct {
	Connected bool   `json:"connected"`
	Name      string `json:"name"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// meDTOFor builds the current-user payload (identity, permissions, AniList link).
func (a *apiV1) meDTOFor(r *http.Request, user *auth.User) meDTO {
	perms := make([]string, 0, len(user.Perms))
	for p := range user.Perms {
		perms = append(perms, p)
	}
	sort.Strings(perms)
	conn := a.svc.AniListConnectionFor(r.Context(), user.ID)
	return meDTO{
		User:        meUserDTO{ID: user.ID, Username: user.Username, Role: user.RoleName, AllowAdult: user.AllowAdult},
		Permissions: perms,
		AniList:     aniListDTO{Connected: conn.Connected, Name: conn.Name, ExpiresAt: conn.ExpiresAt},
	}
}

func (a *apiV1) meta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, metaDTO{
		ServerVersion: serverVersion(),
		APIVersion:    1,
		AuthRequired:  authEnabled(),
		Features:      []string{"archives", "delta_sync", "progress_batch", "collections", "screens", "anilist"},
		ImageFormats:  []string{"jpg", "jpeg", "png", "webp", "gif", "avif"},
		MaxPageBytes:  maxPageBytes,
		InstanceID:    a.instanceID,
	})
}

// loginRL is a fixed-window per-IP limiter for the login endpoint.
type ipWindow struct {
	n   int
	win time.Time
}

var loginRL = struct {
	sync.Mutex
	hits map[string]ipWindow
}{hits: map[string]ipWindow{}}

func loginAllowedIP(ip string) bool {
	const window, max = 15 * time.Minute, 20
	loginRL.Lock()
	defer loginRL.Unlock()
	h := loginRL.hits[ip]
	if time.Since(h.win) > window {
		h = ipWindow{win: time.Now()}
	}
	h.n++
	loginRL.hits[ip] = h
	return h.n <= max
}

// login verifies credentials and mints a never-expiring device API token.
func (a *apiV1) login(w http.ResponseWriter, r *http.Request) {
	if !loginAllowedIP(clientIP(r)) {
		v1err(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts, try again later")
		return
	}
	if !authEnabled() {
		v1err(w, http.StatusBadRequest, "bad_request", "server is in single-user mode; login is not required")
		return
	}
	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
		DeviceID   string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	id, err := a.svc.Auth().Authenticate(r.Context(), body.Username, body.Password)
	if err != nil {
		v1err(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	name := body.DeviceName
	if name == "" {
		name = "iOS app"
	}
	if err := a.svc.Auth().DeleteAPITokensForDevice(r.Context(), id, body.DeviceID); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	token, err := a.svc.Auth().CreateAPIToken(r.Context(), id, name, body.DeviceID, appTokenTTLDays)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	user, err := a.svc.Auth().GetUser(r.Context(), id)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "me": a.meDTOFor(r, user)})
}

func (a *apiV1) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.meDTOFor(r, userFrom(r.Context())))
}

// revokeToken deletes the calling device's API token (sign out).
func (a *apiV1) revokeToken(w http.ResponseWriter, r *http.Request) {
	if token := headerToken(r); token != "" {
		if err := a.svc.Auth().RevokeAPIToken(r.Context(), token); err != nil {
			v1err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// coverClient fetches remote covers with the private-network guard so the proxy
// can't be aimed at internal services.
var coverClient, _ = util.NewHTTPClient(util.HTTPClientOptions{Timeout: 20 * time.Second, BlockPrivateNetworks: true})

// serveChapterPage streams one page image from a downloaded chapter's CBZ.
func serveChapterPage(w http.ResponseWriter, r *http.Request, svc *service.JobService) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter id")
		return
	}
	page, err := parseIntPath(r, "page")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	status, err := svc.ChapterReadStatus(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), svc, status.TitleID) {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	if !status.Downloaded || status.OutputFile == "" {
		writeError(w, http.StatusNotFound, "chapter is not downloaded")
		return
	}
	file, rc, err := cbzPage(status.OutputFile, page)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer rc.Close()
	etag := strconv.Quote(strconv.FormatUint(uint64(file.CRC32), 16))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(file.Name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Length", strconv.FormatUint(file.UncompressedSize64, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// serveVolumePage streams one page image from a volume CBZ.
func serveVolumePage(w http.ResponseWriter, r *http.Request, svc *service.JobService) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid volume id")
		return
	}
	page, err := strconv.Atoi(r.PathValue("page"))
	if err != nil || page <= 0 {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	vol, err := svc.GetVolume(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), svc, vol.TitleID) {
		writeError(w, http.StatusNotFound, "volume not found")
		return
	}
	entry, rc, err := cbzPage(vol.File, page)
	if err != nil {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", imageMime(entry.Name))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = io.Copy(w, rc)
}

// serveVolumeCover serves a volume's custom cover, thumbnail, or first page.
func serveVolumeCover(w http.ResponseWriter, r *http.Request, svc *service.JobService) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vol, err := svc.GetVolume(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), svc, vol.TitleID) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	if blob, m, err := svc.VolumeCover(r.Context(), id); err == nil && len(blob) > 0 {
		w.Header().Set("Content-Type", m)
		_, _ = w.Write(blob)
		return
	}
	if blob, m, err := svc.VolumeThumb(r.Context(), id); err == nil && len(blob) > 0 {
		w.Header().Set("Content-Type", m)
		_, _ = w.Write(blob)
		return
	}
	entry, rc, err := cbzPage(vol.File, 1)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", imageMime(entry.Name))
	_, _ = io.Copy(w, rc)
}

// titleCover proxies a title's remote cover so the app only ever talks to this
// server. ponytail: no persistent cache; relies on the client + Cache-Control.
func (a *apiV1) titleCover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	serveTitleCover(w, r, a.svc, id)
}

// serveTitleCover serves a custom cover blob when set, else proxies the
// remote catalog cover.
func serveTitleCover(w http.ResponseWriter, r *http.Request, svc *service.JobService, id int64) {
	t, err := svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), t.IsAdult, t.ContentTags) {
		http.NotFound(w, r)
		return
	}
	if blob, mime, err := svc.TitleCover(r.Context(), id); err == nil && len(blob) > 0 {
		if mime == "" {
			mime = "image/jpeg"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = w.Write(blob)
		return
	}
	if !strings.HasPrefix(t.CoverImage, "https://") {
		http.NotFound(w, r)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, t.CoverImage, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resp, err := coverClient.Do(req)
	if err != nil {
		http.Error(w, "cover fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "cover unavailable", http.StatusBadGateway)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	_, _ = io.Copy(w, resp.Body)
}

func (a *apiV1) libraryList(w http.ResponseWriter, r *http.Request) {
	now := serverTime()
	titles, err := a.svc.ListTitles(r.Context())
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	titles = filterRestrictedTitles(r.Context(), titles)
	q := r.URL.Query()
	since, ok := parseSince(r)
	if !ok {
		v1err(w, http.StatusBadRequest, "bad_request", "since must be RFC3339")
		return
	}
	var ids []int64
	if since != "" {
		ids = make([]int64, 0, len(titles))
		kept := titles[:0]
		for _, t := range titles {
			ids = append(ids, t.ID)
			if database.FormatTime(t.UpdatedAt) >= since {
				kept = append(kept, t)
			}
		}
		titles = kept
	}
	if rawIDs := q.Get("ids"); rawIDs != "" {
		want := map[int64]bool{}
		for _, part := range strings.Split(rawIDs, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
				want[id] = true
			}
		}
		kept := titles[:0]
		for _, t := range titles {
			if want[t.ID] {
				kept = append(kept, t)
			}
		}
		titles = kept
	}
	titles = filterTitles(titles, libraryControlsFromQuery(q))
	sortTitles(titles, v1SortKey(q.Get("sort")), q.Get("dir"))
	total := len(titles)
	limit := clampLimit(q.Get("limit"))
	off, _ := strconv.Atoi(q.Get("cursor"))
	if off < 0 {
		off = 0
	}
	next := ""
	if off < total {
		end := off + limit
		if end >= total {
			end = total
		} else {
			next = strconv.Itoa(end)
		}
		titles = titles[off:end]
	} else {
		titles = nil
	}
	items := make([]titleDTO, 0, len(titles))
	for _, t := range titles {
		items = append(items, toTitleDTO(t))
	}
	resp := map[string]any{"items": items, "next_cursor": next, "total": total, "server_time": now}
	if ids != nil {
		resp["ids"] = ids
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *apiV1) libraryGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	t, err := a.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), t.IsAdult, t.ContentTags) {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	writeJSON(w, http.StatusOK, toTitleDTO(t))
}

func (a *apiV1) readerProgress(r *http.Request, id int64) (library.TitleReadProgress, error) {
	if r.URL.Query().Get("mode") == "volumes" {
		return a.svc.VolumesReaderProgress(r.Context(), id)
	}
	return a.svc.ReaderProgress(r.Context(), id)
}

func (a *apiV1) readerTitle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	if !titleAllowed(r.Context(), a.svc, id) {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	p, err := a.readerProgress(r, id)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	dto := toTitleReadProgressDTO(p)
	if r.URL.Query().Get("mode") != "volumes" {
		if all, err := a.svc.TitleReadStatuses(r.Context(), id); err == nil {
			dto.Chapters = make([]chapterProgressDTO, len(all))
			for i, c := range all {
				dto.Chapters[i] = toChapterProgressDTO(c)
			}
			dto.TotalChapters = len(all)
		}
	}
	if p.Title.CatalogMangaID != nil {
		if m, err := a.svc.GetManga(r.Context(), *p.Title.CatalogMangaID); err == nil {
			dto.Manga = toMangaDetailDTO(m)
		}
	}
	writeJSON(w, http.StatusOK, dto)
}

func (a *apiV1) manifest(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	if !titleAllowed(r.Context(), a.svc, id) {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	volumes := r.URL.Query().Get("mode") == "volumes"
	p, err := a.readerProgress(r, id)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var m readerManifestResponse
	if c, _ := strconv.ParseInt(r.URL.Query().Get("chapter"), 10, 64); c > 0 {
		m, _, _ = readerManifestWindowMode(p, c, volumes)
	} else {
		m = readerManifest(p)
	}
	rebaseManifest(&m)
	writeJSON(w, http.StatusOK, m)
}

// rebaseManifest rewrites the manifest's /api/reader URLs to their /api/v1 forms.
func rebaseManifest(m *readerManifestResponse) {
	const old, new = "/api/reader/", "/api/v1/reader/"
	m.MarkBase = strings.Replace(m.MarkBase, old, new, 1)
	m.ExtendBase = strings.Replace(m.ExtendBase, old, new, 1)
	for i := range m.Chapters {
		for j := range m.Chapters[i].Pages {
			m.Chapters[i].Pages[j].URL = strings.Replace(m.Chapters[i].Pages[j].URL, old, new, 1)
		}
	}
}

func (a *apiV1) markPage(w http.ResponseWriter, r *http.Request) {
	id, ok := a.guardedChapterID(w, r)
	if !ok {
		return
	}
	var body struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	cs, err := a.svc.MarkPageRead(r.Context(), id, body.Page, body.TotalPages)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toChapterProgressDTO(cs))
}

func (a *apiV1) markComplete(w http.ResponseWriter, r *http.Request) {
	a.chapterMark(w, r, a.svc.MarkChapterRead)
}

func (a *apiV1) markUnread(w http.ResponseWriter, r *http.Request) {
	a.chapterMark(w, r, a.svc.MarkChapterUnread)
}

func (a *apiV1) chapterMark(w http.ResponseWriter, r *http.Request, fn func(context.Context, int64) (library.ChapterReadStatus, error)) {
	id, ok := a.guardedChapterID(w, r)
	if !ok {
		return
	}
	cs, err := fn(r.Context(), id)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toChapterProgressDTO(cs))
}

func (a *apiV1) readRange(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid title id")
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
		Read bool   `json:"read"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if !titleAllowed(r.Context(), a.svc, id) {
		v1err(w, http.StatusNotFound, "not_found", "title not found")
		return
	}
	var count int
	if body.Read {
		count, err = a.svc.MarkChapterRangeRead(r.Context(), id, body.From, body.To)
	} else {
		count, err = a.svc.MarkChapterRangeUnread(r.Context(), id, body.From, body.To)
	}
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"chapters": count})
}

func (a *apiV1) markVolumePage(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid volume id")
		return
	}
	var body struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if vol, err := a.svc.GetVolume(r.Context(), id); err != nil || !titleAllowed(r.Context(), a.svc, vol.TitleID) {
		v1err(w, http.StatusNotFound, "not_found", "volume not found")
		return
	}
	vol, err := a.svc.MarkVolumePageRead(r.Context(), id, body.Page, body.TotalPages)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toVolumeDTO(vol))
}

func clampLimit(s string) int {
	n, _ := strconv.Atoi(s)
	switch {
	case n <= 0:
		return 50
	case n > 200:
		return 200
	default:
		return n
	}
}

// libraryControlsFromQuery maps the v1 query params onto the shared web filter.
func libraryControlsFromQuery(q url.Values) libraryControls {
	c := libraryControls{Q: q.Get("q"), Sources: selectedSources(q["source"]), Progress: q.Get("progress"), Content: q.Get("content")}
	switch q.Get("monitored") {
	case "1":
		c.Monitor = "on"
	case "0":
		c.Monitor = "off"
	}
	if q.Get("favourite") == "1" {
		c.Fav = "only"
	}
	if v := q.Get("include_tags"); v != "" {
		c.IncludeTags = strings.Split(v, ",")
	}
	if v := q.Get("exclude_tags"); v != "" {
		c.ExcludeTags = strings.Split(v, ",")
	}
	return c
}

// v1SortKey maps the v1 "score" sort alias onto the web "rating" key.
func v1SortKey(k string) string {
	if k == "score" {
		return "rating"
	}
	return k
}
