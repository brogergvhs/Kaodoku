package library

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/auth"
)

// Volume is one bound volume file of a title, with the acting user's read state.
type Volume struct {
	ID          int64
	TitleID     int64
	Number      float64
	Name        string
	File        string // absolute path to the .cbz
	Bytes       int64
	Pages       int
	CustomCover bool
	Read        bool
	ReadPages   int
	LastPage    int
}

var reVolumeFile = regexp.MustCompile(`(?i)^\s*vol(?:ume)?\.?\s*([0-9]+(?:\.[0-9]+)?)\s*[-–—:.]*\s*(.*)$`)

// ParseVolumeFile derives a volume number and name from a .cbz filename.
func ParseVolumeFile(name string) (number float64, volName string) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if m := reVolumeFile.FindStringSubmatch(stem); m != nil {
		number, _ = strconv.ParseFloat(m[1], 64)
		return number, strings.TrimSpace(m[2])
	}
	if m := regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*[-–—:.]*\s*(.*)$`).FindStringSubmatch(stem); m != nil {
		number, _ = strconv.ParseFloat(m[1], 64)
		return number, strings.TrimSpace(m[2])
	}
	return 0, strings.TrimSpace(stem)
}

// SyncVolumeFiles upserts volume rows for the .cbz files in dir and removes
// rows whose files vanished. pageCount is injected to keep zip handling out
// of the repository.
func (r *Repository) SyncVolumeFiles(ctx context.Context, titleID int64, dir string, pageCount func(string) int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read volumes dir: %w", err)
	}
	seen := map[string]bool{}
	added := 0
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".cbz") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		seen[path] = true
		num, name := ParseVolumeFile(e.Name())
		var size int64
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
		res, err := r.db.ExecContext(ctx, `
			INSERT INTO volumes (title_id, number, name, file, bytes, pages) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(title_id, file) DO UPDATE SET number = excluded.number, name = excluded.name, bytes = excluded.bytes, pages = excluded.pages
		`, titleID, num, name, path, size, pageCount(path))
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, file FROM volumes WHERE title_id = ?`, titleID)
	if err != nil {
		return added, err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var file string
		if rows.Scan(&id, &file) == nil && !seen[file] {
			if _, err := os.Stat(file); os.IsNotExist(err) {
				stale = append(stale, id)
			}
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := r.db.ExecContext(ctx, `DELETE FROM volumes WHERE id = ?`, id); err != nil {
			return added, err
		}
	}
	return added, nil
}

// Volumes lists a title's volumes with the acting user's read marks.
func (r *Repository) Volumes(ctx context.Context, titleID int64) ([]Volume, error) {
	return r.listVolumes(ctx, `LEFT JOIN volume_read_progress vr ON vr.volume_id = v.id AND vr.user_id = ? WHERE v.title_id = ?`, auth.UserID(ctx), titleID)
}

// VolumesReadSince returns the user's volume progress rows touched after since.
func (r *Repository) VolumesReadSince(ctx context.Context, since string) ([]Volume, error) {
	return r.listVolumes(ctx, `JOIN volume_read_progress vr ON vr.volume_id = v.id AND vr.user_id = ? WHERE vr.last_read_at >= ?`, auth.UserID(ctx), since)
}

func (r *Repository) listVolumes(ctx context.Context, joinWhere string, args ...any) ([]Volume, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT v.id, v.title_id, v.number, v.name, v.file, v.bytes, v.pages, v.cover IS NOT NULL,
			COALESCE(vr.completed, 0), COALESCE(vr.read_pages, 0), COALESCE(vr.last_page, 0)
		FROM volumes v
		`+joinWhere+`
		ORDER BY v.number, v.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		var custom, read int
		if err := rows.Scan(&v.ID, &v.TitleID, &v.Number, &v.Name, &v.File, &v.Bytes, &v.Pages, &custom, &read, &v.ReadPages, &v.LastPage); err != nil {
			return nil, err
		}
		v.CustomCover = custom != 0
		v.Read = read != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// HasVolumes reports whether a title has any volume rows.
func (r *Repository) HasVolumes(ctx context.Context, titleID int64) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM volumes WHERE title_id = ?`, titleID).Scan(&n)
	return n > 0, err
}

// GetVolume returns one volume row (no read state).
func (r *Repository) GetVolume(ctx context.Context, id int64) (Volume, error) {
	var v Volume
	var custom int
	err := r.db.QueryRowContext(ctx, `SELECT id, title_id, number, name, file, bytes, pages, cover IS NOT NULL FROM volumes WHERE id = ?`, id).
		Scan(&v.ID, &v.TitleID, &v.Number, &v.Name, &v.File, &v.Bytes, &v.Pages, &custom)
	v.CustomCover = custom != 0
	return v, err
}

// SetVolumeRead marks a volume read or unread for the acting user.
func (r *Repository) SetVolumeRead(ctx context.Context, volumeID int64, read bool) error {
	if !read {
		_, err := r.db.ExecContext(ctx, `DELETE FROM volume_read_progress WHERE user_id = ? AND volume_id = ?`, auth.UserID(ctx), volumeID)
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO volume_read_progress (user_id, volume_id, completed) VALUES (?, ?, 1)
		ON CONFLICT(user_id, volume_id) DO UPDATE SET completed = 1, last_read_at = CURRENT_TIMESTAMP`, auth.UserID(ctx), volumeID)
	return err
}

// VolumeCover returns the custom cover blob, or nil when none is set.
func (r *Repository) VolumeCover(ctx context.Context, id int64) ([]byte, string, error) {
	var blob []byte
	var mime string
	err := r.db.QueryRowContext(ctx, `SELECT cover, cover_type FROM volumes WHERE id = ?`, id).Scan(&blob, &mime)
	if err == sql.ErrNoRows {
		return nil, "", nil
	}
	return blob, mime, err
}

// SetVolumeCover stores a custom cover; nil blob reverts to the first page.
func (r *Repository) SetVolumeCover(ctx context.Context, id int64, blob []byte, mime string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE volumes SET cover = ?, cover_type = ? WHERE id = ?`, blob, mime, id)
	return err
}

// TitleCover returns a title's custom cover blob, or nil when none is set.
func (r *Repository) TitleCover(ctx context.Context, id int64) ([]byte, string, error) {
	var blob []byte
	var mime string
	err := r.db.QueryRowContext(ctx, `SELECT cover, cover_type FROM titles WHERE id = ?`, id).Scan(&blob, &mime)
	if err == sql.ErrNoRows {
		return nil, "", nil
	}
	return blob, mime, err
}

// SetTitleCover stores a custom title cover; nil blob reverts to the catalog cover.
func (r *Repository) SetTitleCover(ctx context.Context, id int64, blob []byte, mime string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE titles SET cover = ?, cover_type = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, blob, mime, id)
	return err
}

// MoveVolumeFiles rewrites stored volume paths from oldDir to newDir after a
// physical move.
func (r *Repository) MoveVolumeFiles(ctx context.Context, titleID int64, oldDir, newDir string) error {
	rows, err := r.db.QueryContext(ctx, `SELECT id, file FROM volumes WHERE title_id = ?`, titleID)
	if err != nil {
		return err
	}
	type mv struct {
		id   int64
		file string
	}
	var moves []mv
	for rows.Next() {
		var m mv
		if rows.Scan(&m.id, &m.file) == nil && strings.HasPrefix(m.file, oldDir+string(os.PathSeparator)) {
			moves = append(moves, m)
		}
	}
	rows.Close()
	for _, m := range moves {
		next := filepath.Join(newDir, filepath.Base(m.file))
		if _, err := r.db.ExecContext(ctx, `UPDATE volumes SET file = ? WHERE id = ?`, next, m.id); err != nil {
			return err
		}
	}
	return nil
}

// SetVolumeRangeRead marks volumes numbered from..to read or unread for the
// acting user.
func (r *Repository) SetVolumeRangeRead(ctx context.Context, titleID int64, from, to float64, read bool) (int, error) {
	if to < from {
		from, to = to, from
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM volumes WHERE title_id = ? AND number BETWEEN ? AND ?`, titleID, from, to)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := r.SetVolumeRead(ctx, id, read); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// VolumesMissingThumbs lists volumes without a generated thumbnail.
func (r *Repository) VolumesMissingThumbs(ctx context.Context, titleID int64) ([]Volume, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, title_id, file FROM volumes WHERE title_id = ? AND thumb IS NULL`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.ID, &v.TitleID, &v.File); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repository) SetVolumeThumb(ctx context.Context, id int64, blob []byte, mime string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE volumes SET thumb = ?, thumb_type = ? WHERE id = ?`, blob, mime, id)
	return err
}

// VolumeThumb returns the generated thumbnail, if any.
func (r *Repository) VolumeThumb(ctx context.Context, id int64) ([]byte, string, error) {
	var blob []byte
	var mime string
	err := r.db.QueryRowContext(ctx, `SELECT thumb, thumb_type FROM volumes WHERE id = ?`, id).Scan(&blob, &mime)
	if err == sql.ErrNoRows {
		return nil, "", nil
	}
	return blob, mime, err
}

// MarkVolumePageRead records reading progress inside a volume; the volume
// completes when every page is read.
func (r *Repository) MarkVolumePageRead(ctx context.Context, volumeID int64, page, totalPages int) (Volume, error) {
	if page <= 0 {
		return Volume{}, fmt.Errorf("page must be positive")
	}
	v, err := r.GetVolume(ctx, volumeID)
	if err != nil {
		return Volume{}, err
	}
	if totalPages <= 0 {
		totalPages = v.Pages
	}
	return v, upsertVolumePage(ctx, r.db.ExecContext, volumeID, page, totalPages)
}

func upsertVolumePage(ctx context.Context, exec func(context.Context, string, ...any) (sql.Result, error), volumeID int64, page, totalPages int) error {
	completed := 0
	if totalPages > 0 && page >= totalPages {
		completed = 1
	}
	_, err := exec(ctx, `
		INSERT INTO volume_read_progress (user_id, volume_id, completed, read_pages, total_pages, last_page)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, volume_id) DO UPDATE SET
			read_pages = MAX(read_pages, excluded.read_pages),
			total_pages = excluded.total_pages,
			last_page = excluded.last_page,
			completed = MAX(completed, excluded.completed),
			last_read_at = CURRENT_TIMESTAMP
	`, auth.UserID(ctx), volumeID, completed, page, totalPages, page)
	return err
}

// VolumesReaderProgress shapes a title's volumes as reader chapters so the
// manifest/window machinery works unchanged for volume reading.
func (r *Repository) VolumesReaderProgress(ctx context.Context, title Title) (TitleReadProgress, error) {
	vols, err := r.Volumes(ctx, title.ID)
	if err != nil {
		return TitleReadProgress{}, err
	}
	out := TitleReadProgress{Title: title, TotalChapters: len(vols)}
	for _, v := range vols {
		label := strconv.FormatFloat(v.Number, 'f', -1, 64)
		st := ChapterReadStatus{
			Downloaded: true,
			OutputFile: v.File,
			Bytes:      v.Bytes,
			Pages:      v.Pages,
			TotalPages: v.Pages,
			ReadPages:  v.ReadPages,
			Completed:  v.Read,
		}
		st.ID = v.ID
		st.Label = label
		st.Title = v.Name
		st.FirstUnreadPage = v.ReadPages + 1
		if st.FirstUnreadPage > v.Pages {
			st.FirstUnreadPage = v.Pages
		}
		out.Chapters = append(out.Chapters, st)
		out.TotalPages += int64(v.Pages)
		out.ReadPages += int64(v.ReadPages)
		if v.Read {
			out.ReadChapters++
		}
		if out.NextChapterID == 0 && !v.Read {
			out.NextChapterID = v.ID
			out.NextPage = st.FirstUnreadPage
		}
	}
	return out, nil
}
