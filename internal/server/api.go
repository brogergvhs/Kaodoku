// Package server exposes the Kaodoku HTTP API.
package server

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// New returns the HTTP API handler.
func New(
	svc *service.JobService,
	runJobs func(context.Context) (service.RunSummary, error),
	verifySource func(context.Context, string) (service.SourceVerifyResult, error),
) http.Handler {
	mux := http.NewServeMux()
	registerUI(mux, svc, runJobs)
	registerAPIV1(mux, svc, runJobs)
	registerOPDS(mux, svc)
	registerKomga(mux, svc)

	mux.HandleFunc("GET /api/reader/titles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r, "id")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid title id")
			return
		}
		progress, err := svc.ReaderProgress(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !contentAllowed(r.Context(), progress.IsAdult, progress.ContentTags) {
			writeError(w, http.StatusNotFound, "title not found")
			return
		}
		writeJSON(w, http.StatusOK, progress)
	})

	mux.HandleFunc("GET /api/reader/titles/{id}/manifest", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r, "id")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid title id")
			return
		}
		progress, err := svc.ReaderProgress(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !contentAllowed(r.Context(), progress.IsAdult, progress.ContentTags) {
			writeError(w, http.StatusNotFound, "title not found")
			return
		}
		volumes := r.URL.Query().Get("mode") == "volumes"
		if volumes {
			progress, err = svc.VolumesReaderProgress(r.Context(), progress.ID)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// ?chapter=N returns the window around that chapter — the reader uses
		// this to keep extending the strip while scrolling.
		if c, _ := strconv.ParseInt(r.URL.Query().Get("chapter"), 10, 64); c > 0 {
			manifest, _, _ := readerManifestWindowMode(progress, c, volumes)
			writeJSON(w, http.StatusOK, manifest)
			return
		}
		writeJSON(w, http.StatusOK, readerManifest(progress))
	})

	mux.HandleFunc("POST /api/reader/volumes/{id}/pages", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r, "id")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid volume id")
			return
		}
		var req struct {
			Page       int `json:"page"`
			TotalPages int `json:"total_pages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		vol, err := svc.GetVolume(r.Context(), id)
		if err != nil || !titleAllowed(r.Context(), svc, vol.TitleID) {
			writeError(w, http.StatusNotFound, "volume not found")
			return
		}
		vol, err = svc.MarkVolumePageRead(r.Context(), id, req.Page, req.TotalPages)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		title, _ := svc.GetTitle(r.Context(), vol.TitleID)
		presence.SetPage(r.Context(), auth.UserID(r.Context()), title.DisplayTitle, "Vol "+strconv.FormatFloat(vol.Number, 'f', -1, 64), req.Page, req.TotalPages)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/reader/volumes/{id}/pages/{page}", func(w http.ResponseWriter, r *http.Request) {
		serveVolumePage(w, r, svc)
	})

	mux.HandleFunc("POST /api/reader/chapters/{id}/pages", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r, "id")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid chapter id")
			return
		}
		var req struct {
			Page       int `json:"page"`
			TotalPages int `json:"total_pages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if status, err := svc.ChapterReadStatus(r.Context(), id); err != nil || !titleAllowed(r.Context(), svc, status.TitleID) {
			writeError(w, http.StatusNotFound, "chapter not found")
			return
		}
		progress, err := svc.MarkPageRead(r.Context(), id, req.Page, req.TotalPages)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		title, _ := svc.GetTitle(r.Context(), progress.TitleID)
		presence.SetPage(r.Context(), auth.UserID(r.Context()), title.DisplayTitle, "Ch "+progress.Label, req.Page, req.TotalPages)
		writeJSON(w, http.StatusOK, progress)
	})

	mux.HandleFunc("GET /api/reader/chapters/{id}/pages/{page}", func(w http.ResponseWriter, r *http.Request) {
		serveChapterPage(w, r, svc)
	})

	mux.HandleFunc("POST /api/reader/chapters/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r, "id")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid chapter id")
			return
		}
		if status, err := svc.ChapterReadStatus(r.Context(), id); err != nil || !titleAllowed(r.Context(), svc, status.TitleID) {
			writeError(w, http.StatusNotFound, "chapter not found")
			return
		}
		progress, err := svc.MarkChapterRead(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		svc.PushAniListEntry(r.Context(), auth.UserID(r.Context()), progress.TitleID)
		writeJSON(w, http.StatusOK, progress)
	})

	mux.HandleFunc("POST /api/reader/chapters/{id}/unread", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r, "id")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid chapter id")
			return
		}
		if status, err := svc.ChapterReadStatus(r.Context(), id); err != nil || !titleAllowed(r.Context(), svc, status.TitleID) {
			writeError(w, http.StatusNotFound, "chapter not found")
			return
		}
		progress, err := svc.MarkChapterUnread(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		svc.PushAniListEntryExact(r.Context(), auth.UserID(r.Context()), progress.TitleID)
		writeJSON(w, http.StatusOK, progress)
	})

	mux.HandleFunc("POST /api/reader/titles/{id}/read-range", func(w http.ResponseWriter, r *http.Request) {
		titleID, err := parseInt64Path(r, "id")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid title id")
			return
		}
		var req struct {
			From string `json:"from"`
			To   string `json:"to"`
			Read bool   `json:"read"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if !titleAllowed(r.Context(), svc, titleID) {
			writeError(w, http.StatusNotFound, "title not found")
			return
		}
		var count int
		if req.Read {
			count, err = svc.MarkChapterRangeRead(r.Context(), titleID, req.From, req.To)
		} else {
			count, err = svc.MarkChapterRangeUnread(r.Context(), titleID, req.From, req.To)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Read {
			svc.PushAniListEntry(r.Context(), auth.UserID(r.Context()), titleID)
		} else {
			svc.PushAniListEntryExact(r.Context(), auth.UserID(r.Context()), titleID)
		}
		writeJSON(w, http.StatusOK, map[string]int{"chapters": count})
	})

	registerAuthRoutes(mux, svc)
	handler := requireUser(csrfGuard(limitBody(mux)), svc)
	return gzipMiddleware(handler)
}

// gzipCompressible reports whether a path's responses benefit from gzip.
// Page images, CBZ archives, and covers are already compressed — recoding
// them wastes CPU and drops Content-Length (breaking client progress).
func gzipCompressible(p string) bool {
	for _, skip := range []string{
		"/api/reader/chapters/",
		"/api/v1/reader/chapters/",
		"/api/v1/reader/volumes/",
		"/api/v1/covers/",
		"/api/v1/banners/",
		"/api/v1/volumes/",
		"/opds/v1.2/image/",
		"/opds/v1.2/download/",
		"/opds/v1.2/covers/",
		"/opds/v1.2/thumb/",
	} {
		if strings.HasPrefix(p, skip) {
			return false
		}
	}
	if strings.HasPrefix(p, "/komga/") &&
		(strings.Contains(p, "/pages/") || strings.HasSuffix(p, "/thumbnail") || strings.HasSuffix(p, "/file")) {
		return false
	}
	return true
}

// gzipMiddleware compresses textual responses (HTML fragments, CSS, JS,
// JSON). Already-compressed binaries are passed through.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			!gzipCompressible(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzip.NewWriter(w)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() { _ = gz.Close() }()
		next.ServeHTTP(gw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.gz.Write(b)
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.Header().Del("Content-Length") // length changes under compression
	g.ResponseWriter.WriteHeader(code)
}

// limitBody caps request bodies; every handler decodes small JSON payloads.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// csrfGuard rejects state-changing requests whose Origin (browsers always send
// it on cross-origin writes) does not match the host. Requests without an
// Origin (curl, machine API clients) are not a CSRF vector and pass through.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if origin := r.Header.Get("Origin"); origin != "" {
				if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
					writeError(w, http.StatusForbidden, "cross-origin request blocked")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

const redactedSecret = "••••••••"

func parseInt64Query(r *http.Request, key string) (int64, error) {
	value := r.URL.Query().Get(key)
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, strconv.ErrSyntax
	}
	return id, nil
}

func parseInt64Path(r *http.Request, key string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		return 0, strconv.ErrSyntax
	}
	return id, nil
}

func parseIntPath(r *http.Request, key string) (int, error) {
	n, err := strconv.Atoi(r.PathValue(key))
	if err != nil || n <= 0 {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}

type readerManifestChapter struct {
	ID        int64                `json:"id"`
	Label     string               `json:"label"`
	Title     string               `json:"title,omitempty"`
	PageCount int                  `json:"page_count"`
	ReadPages int                  `json:"read_pages"`
	Completed bool                 `json:"completed"`
	Pages     []readerManifestPage `json:"pages"`
}

type readerManifestPage struct {
	Page int       `json:"page"`
	URL  string    `json:"url"`
	Read bool      `json:"read"`
	Crop []float64 `json:"crop,omitempty"`
}

// pageCropsCache caches per-archive crops keyed by path; mtime invalidates.
var pageCropsCache sync.Map

type pageCropsCacheEntry struct {
	mtime time.Time
	crops [][4]float64
}

// archivePageCrops reads the crops entry from a CBZ, cached by path+mtime.
// nil for archives without one (pre-feature downloads, imports, volumes).
func archivePageCrops(path string) [][4]float64 {
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if v, ok := pageCropsCache.Load(path); ok {
		if e := v.(pageCropsCacheEntry); e.mtime.Equal(st.ModTime()) {
			return e.crops
		}
	}
	var crops [][4]float64
	if zr, err := zip.OpenReader(path); err == nil {
		crops = util.ReadPageCrops(zr.File)
		zr.Close()
	}
	pageCropsCache.Store(path, pageCropsCacheEntry{mtime: st.ModTime(), crops: crops})
	return crops
}

type readerManifestResponse struct {
	TitleID          int64                   `json:"title_id"`
	Title            string                  `json:"title"`
	CurrentChapterID int64                   `json:"current_chapter_id,omitempty"`
	ResumeChapterID  int64                   `json:"resume_chapter_id,omitempty"`
	ResumePage       int                     `json:"resume_page,omitempty"`
	MarkBase         string                  `json:"mark_base"`
	ExtendBase       string                  `json:"extend_base"`
	Chapters         []readerManifestChapter `json:"chapters"`
}

const (
	chapterMarkBase   = "/api/reader/chapters/"
	volumeMarkBase    = "/api/reader/volumes/"
	chapterExtendBase = "/api/reader/titles/%d/manifest?chapter="
	volumeExtendBase  = "/api/reader/titles/%d/manifest?mode=volumes&chapter="
)

func readerManifest(progress library.TitleReadProgress) readerManifestResponse {
	return readerManifestFor(progress, progress.Chapters, progress.NextChapterID, progress.NextPage, false)
}

func readerManifestWindowMode(progress library.TitleReadProgress, requestedChapterID int64, volumes bool) (readerManifestResponse, int64, int64) {
	if len(progress.Chapters) == 0 {
		return readerManifestFor(progress, nil, 0, 0, volumes), 0, 0
	}
	current := 0
	target := requestedChapterID
	if target == 0 {
		target = progress.NextChapterID
	}
	if target != 0 {
		for i, chapter := range progress.Chapters {
			if chapter.ID == target {
				current = i
				break
			}
		}
	} else {
		current = len(progress.Chapters) - 1
	}

	resumeChapterID := progress.Chapters[current].ID
	resumePage := progress.Chapters[current].FirstUnreadPage
	if resumePage <= 0 {
		resumePage = 1
	}
	if requestedChapterID == 0 && progress.NextChapterID != 0 {
		resumeChapterID = progress.NextChapterID
		resumePage = progress.NextPage
	}

	// Hard cutoff: the strip starts at the selected chapter (no previous
	// chapters above it); the next chapter is included for continuous reading.
	start, end := current, current+2
	if end > len(progress.Chapters) {
		end = len(progress.Chapters)
	}
	var prevID, nextID int64
	if current > 0 {
		prevID = progress.Chapters[current-1].ID
	}
	if current+1 < len(progress.Chapters) {
		nextID = progress.Chapters[current+1].ID
	}
	return readerManifestFor(progress, progress.Chapters[start:end], resumeChapterID, resumePage, volumes), prevID, nextID
}

func readerManifestFor(progress library.TitleReadProgress, chapters []library.ChapterReadStatus, resumeChapterID int64, resumePage int, volumes bool) readerManifestResponse {
	markBase, extendBase := chapterMarkBase, fmt.Sprintf(chapterExtendBase, progress.ID)
	if volumes {
		markBase, extendBase = volumeMarkBase, fmt.Sprintf(volumeExtendBase, progress.ID)
	}
	out := readerManifestResponse{
		TitleID:          progress.ID,
		Title:            progress.DisplayTitle,
		CurrentChapterID: resumeChapterID,
		ResumeChapterID:  resumeChapterID,
		ResumePage:       resumePage,
		MarkBase:         markBase,
		ExtendBase:       extendBase,
		Chapters:         make([]readerManifestChapter, 0, len(chapters)),
	}
	for _, chapter := range chapters {
		pageCount := chapter.TotalPages
		if pageCount < chapter.Pages {
			pageCount = chapter.Pages
		}
		item := readerManifestChapter{
			ID:        chapter.ID,
			Label:     chapter.Label,
			Title:     chapter.Title,
			PageCount: pageCount,
			ReadPages: chapter.ReadPages,
			Completed: chapter.Completed,
			Pages:     make([]readerManifestPage, 0, pageCount),
		}
		var crops [][4]float64
		if chapter.Downloaded {
			crops = archivePageCrops(chapter.OutputFile)
		}
		for page := 1; page <= pageCount; page++ {
			p := readerManifestPage{
				Page: page,
				URL:  markBase + strconv.FormatInt(chapter.ID, 10) + "/pages/" + strconv.Itoa(page),
				Read: chapter.Completed || page <= chapter.ReadPages,
			}
			if page <= len(crops) && crops[page-1] != util.FullPageCrop {
				c := crops[page-1]
				p.Crop = c[:]
			}
			item.Pages = append(item.Pages, p)
		}
		out.Chapters = append(out.Chapters, item)
	}
	return out
}

func cbzPage(path string, page int) (*zip.File, io.ReadCloser, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, nil, err
	}
	entries := util.CBZImageEntries(zr.File)
	if page > len(entries) {
		zr.Close()
		return nil, nil, strconv.ErrSyntax
	}
	rc, err := entries[page-1].Open()
	if err != nil {
		zr.Close()
		return nil, nil, err
	}
	return entries[page-1], readCloserFunc{
		Reader: rc,
		close: func() error {
			return errors.Join(rc.Close(), zr.Close())
		},
	}, nil
}

type readCloserFunc struct {
	io.Reader
	close func() error
}

func (r readCloserFunc) Close() error {
	return r.close()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
