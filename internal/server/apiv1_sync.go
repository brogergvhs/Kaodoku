package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
)

// serveArchive streams a CBZ with Range/If-Modified-Since support.
func serveArchive(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		v1err(w, http.StatusNotFound, "not_found", "archive not available")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/vnd.comicbook+zip")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

func (a *apiV1) chapterArchive(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	status, err := a.svc.ChapterReadStatus(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), a.svc, status.TitleID) {
		v1err(w, http.StatusNotFound, "not_found", "chapter not found")
		return
	}
	if !status.Downloaded || status.OutputFile == "" {
		v1err(w, http.StatusNotFound, "not_found", "chapter is not downloaded")
		return
	}
	serveArchive(w, r, status.OutputFile)
}

func (a *apiV1) volumeArchive(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		v1err(w, http.StatusBadRequest, "bad_request", "invalid id")
		return
	}
	vol, err := a.svc.GetVolume(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), a.svc, vol.TitleID) {
		v1err(w, http.StatusNotFound, "not_found", "volume not found")
		return
	}
	serveArchive(w, r, vol.File)
}

// parseSince converts an RFC3339 query param to the DB timestamp format.
func parseSince(r *http.Request) (string, bool) {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return "", true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", false
	}
	return database.FormatTime(t), true
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339) }

func (a *apiV1) progressSince(w http.ResponseWriter, r *http.Request) {
	since, ok := parseSince(r)
	if !ok {
		v1err(w, http.StatusBadRequest, "bad_request", "since must be RFC3339")
		return
	}
	now := serverTime()
	chs, vols, err := a.svc.ProgressSince(r.Context(), since)
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	allowed := map[int64]bool{}
	titleOK := func(id int64) bool {
		if v, seen := allowed[id]; seen {
			return v
		}
		allowed[id] = titleAllowed(r.Context(), a.svc, id)
		return allowed[id]
	}
	outC := make([]chapterProgressDTO, 0, len(chs))
	for _, c := range chs {
		if titleOK(c.TitleID) {
			outC = append(outC, toChapterProgressDTO(c))
		}
	}
	outV := make([]volumeDTO, 0, len(vols))
	for _, v := range vols {
		if titleOK(v.TitleID) {
			outV = append(outV, toVolumeDTO(v))
		}
	}
	chIDs, volIDs, err := a.svc.ReadProgressIDs(r.Context())
	if err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chapters": outC, "volumes": outV,
		"chapter_ids": chIDs, "volume_ids": volIDs,
		"server_time": now,
	})
}

// progressBatch replays queued offline reads; chapter entries keep their
// original read_at so time-based metrics stay truthful.
func (a *apiV1) progressBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Entries []struct {
			ChapterID  int64  `json:"chapter_id"`
			VolumeID   int64  `json:"volume_id"`
			Page       int    `json:"page"`
			TotalPages int    `json:"total_pages"`
			ReadAt     string `json:"read_at"`
		} `json:"entries"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	skipped := map[string]int{}
	verdicts := map[[2]int64]string{}
	var marks []library.ReadMark
	for _, e := range body.Entries {
		readAt := ""
		if e.ReadAt != "" {
			t, err := time.Parse(time.RFC3339, e.ReadAt)
			if err != nil || t.Year() < 2000 {
				skipped["bad_time"]++
				continue
			}
			if now := time.Now(); t.After(now) {
				t = now
			}
			readAt = database.FormatTime(t)
		}
		if e.ChapterID <= 0 && e.VolumeID <= 0 {
			skipped["no_id"]++
			continue
		}
		key := [2]int64{e.ChapterID, e.VolumeID}
		why, seen := verdicts[key]
		if !seen {
			why = a.readMarkSkip(r.Context(), e.ChapterID, e.VolumeID)
			verdicts[key] = why
		}
		if why == "" && e.Page <= 0 {
			why = "write_failed"
		}
		if why != "" {
			skipped[why]++
			continue
		}
		marks = append(marks, library.ReadMark{ChapterID: e.ChapterID, VolumeID: e.VolumeID, Page: e.Page, TotalPages: e.TotalPages, ReadAt: readAt})
	}
	if err := a.svc.ApplyReadMarks(r.Context(), marks); err != nil {
		v1err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": len(marks), "received": len(body.Entries), "skipped": skipped,
	})
}

func (a *apiV1) readMarkSkip(ctx context.Context, chapterID, volumeID int64) string {
	var titleID int64
	if chapterID > 0 {
		status, err := a.svc.ChapterReadStatus(ctx, chapterID)
		if err != nil {
			return "unknown_chapter"
		}
		titleID = status.TitleID
	} else {
		vol, err := a.svc.GetVolume(ctx, volumeID)
		if err != nil {
			return "unknown_volume"
		}
		titleID = vol.TitleID
	}
	if !titleAllowed(ctx, a.svc, titleID) {
		return "blocked_title"
	}
	return ""
}
