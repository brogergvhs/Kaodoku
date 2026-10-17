package library

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/database"
)

// Repository persists tracked titles and chapters.
type Repository struct {
	db *sql.DB
	// MaxDownloadAttempts caps failed retries before a chapter stops
	// counting as missing.
	MaxDownloadAttempts int
}

// CompletedDownload is a persisted completed download record.
type CompletedDownload struct {
	ChapterID  int64
	TitleID    int64
	Label      string
	OutputFile string
}

// NewRepository creates a library repository.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db, MaxDownloadAttempts: DefaultDownloadAttempts}
}

// AddTitle creates or updates a tracked title by source URL.
func (r *Repository) AddTitle(ctx context.Context, params AddTitleParams) (Title, error) {
	params = normalizeTitleParams(params)
	if params.SourceURL == "" {
		return Title{}, errors.New("source URL cannot be empty")
	}
	if _, err := url.ParseRequestURI(params.SourceURL); err != nil {
		return Title{}, fmt.Errorf("invalid source URL: %w", err)
	}
	// The download root is only known at download time; reject traversal
	// segments here so escapes are never even stored.
	if hasDotDot(params.OutputPath) {
		return Title{}, fmt.Errorf(`output path %q must not contain ".."`, params.OutputPath)
	}

	var catalogID any
	if params.CatalogMangaID != nil {
		catalogID = *params.CatalogMangaID
	}
	var sourceID any
	if params.SourceID != "" {
		sourceID = params.SourceID
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO titles (
			catalog_manga_id,
			source_id,
			source_url,
			display_title,
			output_path,
			monitored,
			refresh_interval,
			added_by,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(source_url) DO UPDATE SET
			catalog_manga_id = COALESCE(excluded.catalog_manga_id, titles.catalog_manga_id),
			source_id = COALESCE(excluded.source_id, titles.source_id),
			display_title = excluded.display_title,
			output_path = excluded.output_path,
			monitored = excluded.monitored,
			refresh_interval = excluded.refresh_interval,
			updated_at = CURRENT_TIMESTAMP
		RETURNING id
	`, catalogID, sourceID, params.SourceURL, params.DisplayTitle, params.OutputPath, database.BoolToInt(params.Monitored), params.RefreshInterval, auth.UserID(ctx))

	var id int64
	if err := row.Scan(&id); err != nil {
		return Title{}, fmt.Errorf("add title: %w", err)
	}

	return r.GetTitle(ctx, id)
}

// GetTitle returns a tracked title by ID.
func (r *Repository) GetTitle(ctx context.Context, id int64) (Title, error) {
	row := r.db.QueryRowContext(ctx, r.titleSelectQuery()+` WHERE t.id = ?`, auth.UserID(ctx), auth.UserID(ctx), auth.UserID(ctx), id)

	title, err := scanTitle(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Title{}, fmt.Errorf("title %d not found", id)
		}
		return Title{}, fmt.Errorf("get title %d: %w", id, err)
	}

	return title, nil
}

// ListTitles returns all tracked titles with chapter counts.
func (r *Repository) ListTitles(ctx context.Context) ([]Title, error) {
	rows, err := r.db.QueryContext(ctx, r.titleSelectQuery()+` ORDER BY t.display_title COLLATE NOCASE, t.id`, auth.UserID(ctx), auth.UserID(ctx), auth.UserID(ctx))
	if err != nil {
		return nil, fmt.Errorf("list titles: %w", err)
	}
	defer rows.Close()

	var out []Title
	for rows.Next() {
		title, err := scanTitle(rows)
		if err != nil {
			return nil, fmt.Errorf("scan title: %w", err)
		}
		out = append(out, title)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate titles: %w", err)
	}

	return out, nil
}

// RemoveTitle deletes a tracked title and its dependent chapters/downloads.
func (r *Repository) RemoveTitle(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM titles WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove title %d: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check removed title %d: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("title %d not found", id)
	}

	return nil
}

// LinkSource repoints a title (e.g. an imported one) at a real source URL.
func (r *Repository) LinkSource(ctx context.Context, id int64, sourceURL, sourceID string) error {
	sourceURL = strings.TrimSpace(sourceURL)
	if sourceURL == "" {
		return errors.New("source URL cannot be empty")
	}
	var sid any
	if strings.TrimSpace(sourceID) != "" {
		sid = strings.TrimSpace(sourceID)
	}
	var other int64
	switch err := r.db.QueryRowContext(ctx, `SELECT id FROM titles WHERE source_url = ? AND id != ?`, sourceURL, id).Scan(&other); {
	case err == nil:
		return fmt.Errorf("that source is already linked to another tracked title")
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("check existing source link: %w", err)
	}
	result, err := r.db.ExecContext(ctx, `UPDATE titles SET source_url = ?, source_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, sourceURL, sid, id)
	if err != nil {
		return fmt.Errorf("link source for title %d: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check source link %d: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("title %d not found", id)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO title_sources (title_id, source_id, url) VALUES (?, ?, ?)`, id, sid, sourceURL); err != nil {
		return fmt.Errorf("record linked source: %w", err)
	}
	return nil
}

// LinkedSource is one source linked to a title.
type LinkedSource struct {
	ID       int64
	SourceID string
	URL      string
}

// ListTitleSources returns every source linked to a title, oldest first.
func (r *Repository) ListTitleSources(ctx context.Context, titleID int64) ([]LinkedSource, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, source_id, url FROM title_sources WHERE title_id = ? ORDER BY created_at, id`, titleID)
	if err != nil {
		return nil, fmt.Errorf("list title sources: %w", err)
	}
	defer rows.Close()
	var out []LinkedSource
	for rows.Next() {
		var ls LinkedSource
		if err := rows.Scan(&ls.ID, &ls.SourceID, &ls.URL); err != nil {
			return nil, fmt.Errorf("scan title source: %w", err)
		}
		out = append(out, ls)
	}
	return out, rows.Err()
}

// UnlinkSource removes a linked source; if it was the active one, another
// linked source is promoted, or the title reverts to having no source.
// UpdateDownloadFile rewrites a completed download's stored path after a
// physical move (metadata untouched).
func (r *Repository) UpdateDownloadFile(ctx context.Context, chapterID int64, file string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE downloads SET output_file = ? WHERE chapter_id = ? AND status = 'completed'`, file, chapterID)
	return err
}

// TitleOwners maps title id to the user id that added it (0 = added before
// ownership was tracked).
func (r *Repository) TitleOwners(ctx context.Context) (map[int64]int64, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, added_by FROM titles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, addedBy int64
		if err := rows.Scan(&id, &addedBy); err != nil {
			return nil, err
		}
		out[id] = addedBy
	}
	return out, rows.Err()
}

// DeleteDownload drops a chapter's download record so the chapter counts as
// missing again, returning the file that backed it for the caller to remove.
func (r *Repository) DeleteDownload(ctx context.Context, chapterID int64) (string, error) {
	var file string
	_ = r.db.QueryRowContext(ctx, `SELECT COALESCE(output_file, '') FROM downloads WHERE chapter_id = ?`, chapterID).Scan(&file)
	if _, err := r.db.ExecContext(ctx, `DELETE FROM downloads WHERE chapter_id = ?`, chapterID); err != nil {
		return "", err
	}
	return file, nil
}

// RenameChapter updates a chapter's descriptive title.
func (r *Repository) RenameChapter(ctx context.Context, chapterID int64, title string) error {
	title = strings.TrimSpace(title)
	_, err := r.db.ExecContext(ctx, `UPDATE chapters SET title = ?,
		title_custom = CASE WHEN ? = scraped_title THEN 0 ELSE 1 END,
		updated_at = CURRENT_TIMESTAMP WHERE id = ?`, title, title, chapterID)
	return err
}

// ResetChapterTitle restores one manually renamed chapter to its scraped name.
func (r *Repository) ResetChapterTitle(ctx context.Context, chapterID int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE chapters SET title = scraped_title, title_custom = 0,
		updated_at = CURRENT_TIMESTAMP WHERE id = ? AND title_custom = 1`, chapterID)
	return err
}

type ChapterRename struct {
	ChapterID int64
	Title     string
	Volume    *float64
}

// BulkRenameChapters applies manual names and volume assignments in one
// transaction.
func (r *Repository) BulkRenameChapters(ctx context.Context, titleID int64, items []ChapterRename) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bulk rename: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	stmt, err := tx.PrepareContext(ctx, `UPDATE chapters SET title = ?,
		title_custom = CASE WHEN ? = scraped_title THEN 0 ELSE 1 END,
		volume_num = CASE WHEN ? THEN ? ELSE volume_num END,
		volume_custom = CASE WHEN ? THEN 1 ELSE volume_custom END,
		updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND title_id = ?`)
	if err != nil {
		return fmt.Errorf("prepare bulk rename: %w", err)
	}
	defer stmt.Close()
	for _, item := range items {
		title := strings.TrimSpace(item.Title)
		hasVol, vol := 0, 0.0
		if item.Volume != nil {
			hasVol, vol = 1, *item.Volume
		}
		res, execErr := stmt.ExecContext(ctx, title, title, hasVol, vol, hasVol, item.ChapterID, titleID)
		if execErr != nil {
			err = fmt.Errorf("rename chapter %d: %w", item.ChapterID, execErr)
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			err = fmt.Errorf("chapter %d does not belong to this title", item.ChapterID)
			return err
		}
	}
	err = tx.Commit()
	return err
}

// ResetChapterTitles restores manually renamed chapters to their scraped names.
func (r *Repository) ResetChapterTitles(ctx context.Context, titleID int64, from, to string) (int, error) {
	if strings.TrimSpace(from) == "" && strings.TrimSpace(to) == "" {
		res, err := r.db.ExecContext(ctx, `UPDATE chapters SET title = scraped_title, title_custom = 0,
			updated_at = CURRENT_TIMESTAMP WHERE title_id = ? AND title_custom = 1`, titleID)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	chapters, err := r.ListChapters(ctx, titleID)
	if err != nil {
		return 0, err
	}
	selected, err := selectChapterRange(chapters, from, to)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, ch := range selected {
		res, err := r.db.ExecContext(ctx, `UPDATE chapters SET title = scraped_title, title_custom = 0,
			updated_at = CURRENT_TIMESTAMP WHERE id = ? AND title_custom = 1`, ch.ID)
		if err != nil {
			return count, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			count++
		}
	}
	return count, nil
}

// SetLanguageMode stores the user's decision for language-gapped titles.
func (r *Repository) SetLanguageMode(ctx context.Context, titleID int64, mode string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE titles SET language_mode = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, mode, titleID)
	return err
}

// SetLanguageGap records how many chapters exist only outside the preferred
// languages (refresh keeps it current).
func (r *Repository) SetLanguageGap(ctx context.Context, titleID int64, gap int) error {
	_, err := r.db.ExecContext(ctx, `UPDATE titles SET language_gap = ? WHERE id = ?`, gap, titleID)
	return err
}

// SetFetchError records a title's last chapter-refresh failure; empty clears it.
func (r *Repository) SetFetchError(ctx context.Context, titleID int64, msg string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE titles SET fetch_error = ? WHERE id = ?`, msg, titleID)
	return err
}

// ToggleFavourite flips the acting user's favourite mark for a title and
// returns the new state.
func (r *Repository) ToggleFavourite(ctx context.Context, titleID int64) (bool, error) {
	userID := auth.UserID(ctx)
	res, err := r.db.ExecContext(ctx, `DELETE FROM user_favourites WHERE user_id = ? AND title_id = ?`, userID, titleID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO user_favourites (user_id, title_id) VALUES (?, ?)`, userID, titleID)
	return true, err
}

// SetFavourite marks a title favourite without toggling (AniList pull merge).
func (r *Repository) SetFavourite(ctx context.Context, userID, titleID int64) error {
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO user_favourites (user_id, title_id) VALUES (?, ?)`, userID, titleID)
	return err
}

// UpdateSourceURL follows a site's URL rotation: the title's active source
// URL and its linked-source row move to the redirect target. OR IGNORE keeps
// a unique-constraint collision (target URL already stored) from failing the
// refresh that discovered the move.
func (r *Repository) UpdateSourceURL(ctx context.Context, titleID int64, oldURL, newURL string) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE OR IGNORE title_sources SET url = ? WHERE title_id = ? AND url = ?`, newURL, titleID, oldURL); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE OR IGNORE titles SET source_url = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND source_url = ?`, newURL, titleID, oldURL)
	return err
}

func (r *Repository) UnlinkSource(ctx context.Context, titleID int64, url string) error {
	url = strings.TrimSpace(url)
	var primary string
	if err := r.db.QueryRowContext(ctx, `SELECT source_url FROM titles WHERE id = ?`, titleID).Scan(&primary); err != nil {
		return fmt.Errorf("load title %d: %w", titleID, err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM title_sources WHERE title_id = ? AND url = ?`, titleID, url); err != nil {
		return fmt.Errorf("unlink source: %w", err)
	}
	if err := r.pruneSourceChapters(ctx, titleID, url); err != nil {
		return err
	}
	if primary != url {
		return nil // removed a non-active source; the active one is unchanged
	}
	var sid, next string
	err := r.db.QueryRowContext(ctx, `SELECT source_id, url FROM title_sources WHERE title_id = ? ORDER BY created_at, id LIMIT 1`, titleID).Scan(&sid, &next)
	switch {
	case err == nil:
		var idAny any
		if strings.TrimSpace(sid) != "" {
			idAny = sid
		}
		_, err = r.db.ExecContext(ctx, `UPDATE titles SET source_url = ?, source_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, idAny, titleID)
		return err
	case errors.Is(err, sql.ErrNoRows):
		_, err = r.db.ExecContext(ctx, `UPDATE titles SET source_url = ?, source_id = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, fmt.Sprintf("pending:t%d", titleID), titleID)
		return err
	default:
		return err
	}
}

// pruneSourceChapters removes a title's chapters discovered from the given
// source URL (matched by host) that have no completed download; downloaded
// chapters always stay.
func (r *Repository) pruneSourceChapters(ctx context.Context, titleID int64, unlinkedURL string) error {
	host := chapterHost(unlinkedURL)
	if host == "" {
		return nil
	}
	// Another linked source on the same host still owns these chapters.
	links, err := r.ListTitleSources(ctx, titleID)
	if err != nil {
		return err
	}
	for _, l := range links {
		if chapterHost(l.URL) == host {
			return nil
		}
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.url FROM chapters c
		LEFT JOIN downloads d ON d.chapter_id = c.id AND d.status = 'completed'
		WHERE c.title_id = ? AND d.id IS NULL
	`, titleID)
	if err != nil {
		return fmt.Errorf("list undownloaded chapters: %w", err)
	}
	defer rows.Close()
	var ids []any
	for rows.Next() {
		var id int64
		var chapterURL string
		if err := rows.Scan(&id, &chapterURL); err != nil {
			return fmt.Errorf("scan chapter: %w", err)
		}
		if chapterHost(chapterURL) == host {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	query := `DELETE FROM chapters WHERE id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
	if _, err := r.db.ExecContext(ctx, query, ids...); err != nil {
		return fmt.Errorf("prune %d chapters: %w", len(ids), err)
	}
	return nil
}

func chapterHost(raw string) string {
	raw, _, _ = strings.Cut(raw, "\t")
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}

// SetRefreshInterval sets a title's custom refresh cadence; empty means follow
// the global schedule.
func (r *Repository) SetRefreshInterval(ctx context.Context, id int64, interval string) error {
	interval = strings.TrimSpace(interval)
	if interval != "" {
		if d, err := time.ParseDuration(interval); err != nil || d <= 0 {
			return fmt.Errorf("invalid refresh interval %q (use e.g. 6h, 30m)", interval)
		}
	}
	result, err := r.db.ExecContext(ctx, `UPDATE titles SET refresh_interval = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, interval, id)
	if err != nil {
		return fmt.Errorf("set refresh interval %d: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("title %d not found", id)
	}
	return nil
}

// SetMonitored toggles monitoring for a title.
func (r *Repository) SetMonitored(ctx context.Context, id int64, monitored bool) error {
	result, err := r.db.ExecContext(ctx, `UPDATE titles SET monitored = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, database.BoolToInt(monitored), id)
	if err != nil {
		return fmt.Errorf("set monitored %d: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check monitored update %d: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("title %d not found", id)
	}
	return nil
}

// UpsertChapters stores discovered chapters for a title.
func (r *Repository) UpsertChapters(
	ctx context.Context,
	titleID int64,
	discovered []chapters.Chapter,
) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin chapter upsert: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO chapters (
			title_id,
			label,
			title,
			scraped_title,
			url,
			number_main,
			suffix_type,
			suffix_num,
			volume_num,
			source_uid,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(title_id, label) DO UPDATE SET
			-- A manual rename survives refreshes; the scraped name is kept
			-- current alongside it so a reset restores the latest one.
			title = CASE WHEN chapters.title_custom = 1 THEN chapters.title ELSE excluded.title END,
			scraped_title = excluded.scraped_title,
			url = excluded.url,
			number_main = excluded.number_main,
			suffix_type = excluded.suffix_type,
			suffix_num = excluded.suffix_num,
			-- Manual volume assignments win; a scrape that lost its volume
			-- info must not zero out one it reported before.
			volume_num = CASE WHEN chapters.volume_custom = 1 OR excluded.volume_num = 0
				THEN chapters.volume_num ELSE excluded.volume_num END,
			source_uid = excluded.source_uid,
			updated_at = CURRENT_TIMESTAMP
	`)
	if err != nil {
		return 0, fmt.Errorf("prepare chapter upsert: %w", err)
	}
	defer stmt.Close()

	displace, err := tx.PrepareContext(ctx, `
		UPDATE chapters SET url = 'stale:' || id, updated_at = CURRENT_TIMESTAMP
		WHERE title_id = ? AND url = ? AND label <> ?
	`)
	if err != nil {
		return 0, fmt.Errorf("prepare chapter displace: %w", err)
	}
	defer displace.Close()

	// Chapters also have UNIQUE(title_id, url); a second label for the same
	// URL would abort the whole transaction, so keep the first one.
	seenURL := map[string]bool{}
	count := 0
	for _, ch := range discovered {
		if strings.TrimSpace(ch.Label) == "" || strings.TrimSpace(ch.URL) == "" || seenURL[ch.URL] {
			continue
		}
		seenURL[ch.URL] = true
		uid := ch.UID
		if uid == "" {
			sum := sha256.Sum256([]byte(ch.URL))
			uid = hex.EncodeToString(sum[:8])
		}
		if _, err = displace.ExecContext(ctx, titleID, ch.URL, ch.Label); err != nil {
			return 0, fmt.Errorf("displace chapter url %q: %w", ch.URL, err)
		}
		if _, err = stmt.ExecContext(
			ctx,
			titleID,
			ch.Label,
			ch.Title,
			ch.Title, // scraped_title
			ch.URL,
			ch.NumMain,
			ch.SuffixType,
			ch.SuffixNum,
			ch.Volume,
			uid,
		); err != nil {
			return 0, fmt.Errorf("upsert chapter %q: %w", ch.Label, err)
		}
		count++
	}

	if _, err = tx.ExecContext(ctx, `UPDATE titles SET last_refreshed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, titleID); err != nil {
		return 0, fmt.Errorf("mark title refreshed: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit chapter upsert: %w", err)
	}

	return count, nil
}

// ResetFailedDownloads gives a title's failed downloads a fresh attempt
// budget, so an explicit re-download retries chapters that hit the cap.
func (r *Repository) ResetFailedDownloads(ctx context.Context, titleID int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE downloads SET attempts = 0, updated_at = CURRENT_TIMESTAMP
		WHERE status = 'failed'
			AND chapter_id IN (SELECT id FROM chapters WHERE title_id = ?)
	`, titleID)
	if err != nil {
		return fmt.Errorf("reset failed downloads for title %d: %w", titleID, err)
	}
	return nil
}

// ListMissingChapters returns discovered chapters without a completed
// download, skipping chapters that already failed MaxDownloadAttempts times.
func (r *Repository) ListMissingChapters(ctx context.Context, titleID int64) ([]Chapter, error) {
	rows, err := r.db.QueryContext(ctx, chapterSelectQuery()+`
		LEFT JOIN downloads d
			ON d.chapter_id = c.id
			AND (d.status = 'completed'
				OR (d.status = 'failed' AND d.attempts >= ?))
		WHERE c.title_id = ?
			AND d.id IS NULL
		ORDER BY c.number_main, c.suffix_type, c.suffix_num, c.label
	`, r.MaxDownloadAttempts, titleID)
	if err != nil {
		return nil, fmt.Errorf("list missing chapters: %w", err)
	}
	defer rows.Close()

	var out []Chapter
	for rows.Next() {
		ch, err := scanChapter(rows)
		if err != nil {
			return nil, fmt.Errorf("scan missing chapter: %w", err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missing chapters: %w", err)
	}

	return out, nil
}

// ChapterStatus is a discovered chapter plus its download state.
type ChapterStatus struct {
	Chapter
	TitleCustom  bool
	VolumeNum    float64
	Downloaded   bool
	Failed       bool
	Attempts     int
	Error        string
	OutputFile   string
	Bytes        int64
	Pages        int
	ReadPages    int
	TotalPages   int
	Read         bool
	DownloadedAt *time.Time
}

// ListChapters returns all discovered chapters for a title with download state.
func (r *Repository) ListChapters(ctx context.Context, titleID int64) ([]ChapterStatus, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.title_id, c.label, c.title, c.title_custom, c.volume_num, c.url, c.number_main, c.suffix_type, c.suffix_num,
			c.discovered_at, c.updated_at,
			COALESCE(d.status, ''), COALESCE(d.attempts, 0), COALESCE(d.error, ''),
			COALESCE(d.output_file, ''), COALESCE(d.bytes, 0), COALESCE(d.pages, 0),
			COALESCE(rp.read_pages, 0), COALESCE(NULLIF(rp.total_pages, 0), d.pages, 0), COALESCE(rp.completed, 0),
			COALESCE(d.completed_at, '')
		FROM chapters c
		LEFT JOIN downloads d ON d.chapter_id = c.id
		LEFT JOIN chapter_read_progress rp ON rp.chapter_id = c.id AND rp.user_id = ?
		WHERE c.title_id = ?
		ORDER BY c.number_main, c.suffix_type, c.suffix_num, c.label
	`, auth.UserID(ctx), titleID)
	if err != nil {
		return nil, fmt.Errorf("list chapters: %w", err)
	}
	defer rows.Close()

	var out []ChapterStatus
	for rows.Next() {
		var cs ChapterStatus
		var discoveredAt, updatedAt, status, completedAt string
		var read int
		if err := rows.Scan(&cs.ID, &cs.TitleID, &cs.Label, &cs.Title, &cs.TitleCustom, &cs.VolumeNum, &cs.URL, &cs.NumberMain,
			&cs.SuffixType, &cs.SuffixNum, &discoveredAt, &updatedAt,
			&status, &cs.Attempts, &cs.Error, &cs.OutputFile, &cs.Bytes, &cs.Pages,
			&cs.ReadPages, &cs.TotalPages, &read, &completedAt); err != nil {
			return nil, fmt.Errorf("scan chapter: %w", err)
		}
		cs.Downloaded = status == "completed"
		if t, err := database.ParseTime(completedAt); err == nil && completedAt != "" {
			cs.DownloadedAt = &t
		}
		cs.Failed = status == "failed" && cs.Attempts >= r.MaxDownloadAttempts
		cs.Read = read != 0
		cs.DiscoveredAt, _ = database.ParseTime(discoveredAt)
		cs.UpdatedAt, _ = database.ParseTime(updatedAt)
		out = append(out, cs)
	}
	return out, rows.Err()
}

func selectChapterRange(chapters []ChapterStatus, from, to string) ([]ChapterStatus, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return nil, fmt.Errorf("chapter range needs from and to")
	}
	fromN, fromErr := strconv.Atoi(from)
	toN, toErr := strconv.Atoi(to)
	if fromErr == nil && toErr == nil {
		if fromN > toN {
			fromN, toN = toN, fromN
		}
		var out []ChapterStatus
		for _, ch := range chapters {
			if ch.NumberMain >= fromN && ch.NumberMain <= toN {
				out = append(out, ch)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	start := chapterRangeIndex(chapters, from, false)
	end := chapterRangeIndex(chapters, to, true)
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("chapter range %q-%q not found", from, to)
	}
	if start > end {
		start, end = end, start
	}
	return chapters[start : end+1], nil
}

func chapterRangeIndex(chapters []ChapterStatus, value string, last bool) int {
	value = normalizeChapterBoundary(value)
	number, err := strconv.Atoi(value)
	hasNumber := err == nil
	found := -1
	for i, ch := range chapters {
		match := normalizeChapterBoundary(ch.Label) == value
		if !match && hasNumber {
			match = ch.NumberMain == number
		}
		if match {
			found = i
			if !last {
				return i
			}
		}
	}
	return found
}

func normalizeChapterBoundary(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"chapter", "episode", "ch."} {
		value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
	}
	return strings.TrimSpace(strings.TrimPrefix(value, "ch"))
}

func retryBusy[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	var err error
	for i := 0; i < 5; i++ {
		var out T
		out, err = fn()
		if err == nil || !isBusyError(err) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(time.Duration(i+1) * 75 * time.Millisecond):
		}
	}
	return zero, err
}

func isBusyError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// ReaderProgress returns downloaded chapters and read progress for a title.
func (r *Repository) ReaderProgress(ctx context.Context, titleID int64) (TitleReadProgress, error) {
	title, err := r.GetTitle(ctx, titleID)
	if err != nil {
		return TitleReadProgress{}, err
	}
	chapters, err := r.listReadChapters(ctx, `WHERE c.title_id = ? AND d.status = 'completed'`, titleID)
	if err != nil {
		return TitleReadProgress{}, err
	}

	out := TitleReadProgress{Title: title, Chapters: chapters, TotalChapters: len(chapters)}
	for _, ch := range chapters {
		out.ReadPages += int64(ch.ReadPages)
		out.TotalPages += int64(ch.TotalPages)
		if ch.Completed {
			out.ReadChapters++
			continue
		}
		if out.NextChapterID == 0 {
			out.NextChapterID = ch.ID
			out.NextPage = ch.FirstUnreadPage
		}
	}
	return out, nil
}

// TitleReadStatuses lists all discovered chapters with download + read state.
func (r *Repository) TitleReadStatuses(ctx context.Context, titleID int64) ([]ChapterReadStatus, error) {
	return r.listReadChapters(ctx, `WHERE c.title_id = ?`, titleID)
}

// MarkPageRead records one completed page and updates the chapter read summary.
func (r *Repository) MarkPageRead(ctx context.Context, chapterID int64, page, totalPages int) (ChapterReadStatus, error) {
	return r.MarkPageReadAt(ctx, chapterID, page, totalPages, "")
}

// MarkPageReadAt marks a page read with an explicit read_at ("" = now) so
// offline reads replay with their original timestamps.
func (r *Repository) MarkPageReadAt(ctx context.Context, chapterID int64, page, totalPages int, readAt string) (ChapterReadStatus, error) {
	if page <= 0 {
		return ChapterReadStatus{}, fmt.Errorf("page must be positive")
	}
	return retryBusy(ctx, func() (ChapterReadStatus, error) {
		return r.markPageRead(ctx, chapterID, page, totalPages, readAt)
	})
}

func (r *Repository) markPageRead(ctx context.Context, chapterID int64, page, totalPages int, readAt string) (ChapterReadStatus, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ChapterReadStatus{}, fmt.Errorf("begin read progress: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var downloadPages int
	if err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(d.pages, 0)
		FROM chapters c
		LEFT JOIN downloads d ON d.chapter_id = c.id
		WHERE c.id = ?
	`, chapterID).Scan(&downloadPages); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChapterReadStatus{}, fmt.Errorf("chapter %d not found", chapterID)
		}
		return ChapterReadStatus{}, fmt.Errorf("load chapter page count: %w", err)
	}
	totalPagesProvided := totalPages > 0
	if !totalPagesProvided {
		totalPages = downloadPages
	}
	if totalPagesProvided && totalPages < page {
		totalPages = page
	}

	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO chapter_read_pages (user_id, chapter_id, page, read_at)
		VALUES (?, ?, ?, COALESCE(NULLIF(?, ''), CURRENT_TIMESTAMP))
	`, auth.UserID(ctx), chapterID, page, readAt)
	if err != nil {
		return ChapterReadStatus{}, fmt.Errorf("mark page read: %w", err)
	}
	inserted, _ := res.RowsAffected()
	if err = r.updateReadProgress(ctx, tx, chapterID, totalPages, false, inserted == 0); err != nil {
		return ChapterReadStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return ChapterReadStatus{}, fmt.Errorf("commit read progress: %w", err)
	}
	return r.GetChapterReadStatus(ctx, chapterID)
}

// MarkChapterRead marks every known page in a chapter and completes it.
func (r *Repository) MarkChapterRead(ctx context.Context, chapterID int64) (ChapterReadStatus, error) {
	return retryBusy(ctx, func() (ChapterReadStatus, error) {
		return r.markChapterRead(ctx, chapterID)
	})
}

func (r *Repository) markChapterRead(ctx context.Context, chapterID int64) (ChapterReadStatus, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ChapterReadStatus{}, fmt.Errorf("begin chapter completion: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var totalPages int
	if err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(d.pages, 0), rp.total_pages, 0)
		FROM chapters c
		LEFT JOIN downloads d ON d.chapter_id = c.id
		LEFT JOIN chapter_read_progress rp ON rp.chapter_id = c.id AND rp.user_id = ?
		WHERE c.id = ?
	`, auth.UserID(ctx), chapterID).Scan(&totalPages); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChapterReadStatus{}, fmt.Errorf("chapter %d not found", chapterID)
		}
		return ChapterReadStatus{}, fmt.Errorf("load chapter completion count: %w", err)
	}
	for page := 1; page <= totalPages; page++ {
		if _, err = tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO chapter_read_pages (user_id, chapter_id, page)
			VALUES (?, ?, ?)
		`, auth.UserID(ctx), chapterID, page); err != nil {
			return ChapterReadStatus{}, fmt.Errorf("mark chapter page %d read: %w", page, err)
		}
	}
	if err = r.updateReadProgress(ctx, tx, chapterID, totalPages, true, true); err != nil {
		return ChapterReadStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return ChapterReadStatus{}, fmt.Errorf("commit chapter completion: %w", err)
	}
	return r.GetChapterReadStatus(ctx, chapterID)
}

// MarkChapterUnread clears read progress for a chapter.
func (r *Repository) MarkChapterUnread(ctx context.Context, chapterID int64) (ChapterReadStatus, error) {
	return retryBusy(ctx, func() (ChapterReadStatus, error) {
		return r.markChapterUnread(ctx, chapterID)
	})
}

func (r *Repository) markChapterUnread(ctx context.Context, chapterID int64) (ChapterReadStatus, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ChapterReadStatus{}, fmt.Errorf("begin chapter unread: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM chapter_read_pages WHERE user_id = ? AND chapter_id = ?`, auth.UserID(ctx), chapterID); err != nil {
		return ChapterReadStatus{}, fmt.Errorf("clear read pages: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM chapter_read_progress WHERE user_id = ? AND chapter_id = ?`, auth.UserID(ctx), chapterID); err != nil {
		return ChapterReadStatus{}, fmt.Errorf("clear read progress: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return ChapterReadStatus{}, fmt.Errorf("commit chapter unread: %w", err)
	}
	return r.GetChapterReadStatus(ctx, chapterID)
}

// MarkChapterRangeRead marks downloaded chapters in an inclusive label/number range read.
func (r *Repository) MarkChapterRangeRead(ctx context.Context, titleID int64, from, to string) (int, error) {
	chapters, err := r.ListChapters(ctx, titleID)
	if err != nil {
		return 0, err
	}
	selected, err := selectChapterRange(chapters, from, to)
	if err != nil {
		return 0, err
	}
	// One transaction for the whole range: per-chapter marking commits (and
	// fsyncs) chapter by chapter, which crawls on large ranges.
	return retryBusy(ctx, func() (int, error) {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin range read: %w", err)
		}
		defer func() {
			if err != nil {
				_ = tx.Rollback()
			}
		}()
		count := 0
		for _, ch := range selected {
			if !ch.Downloaded {
				continue
			}
			total := ch.TotalPages
			if total <= 0 {
				total = ch.Pages
			}
			if total > 0 {
				// set-based page fill: one statement per chapter, not per page
				if _, err = tx.ExecContext(ctx, `
					INSERT OR IGNORE INTO chapter_read_pages (user_id, chapter_id, page)
					WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
					SELECT ?, ?, n FROM seq
				`, total, auth.UserID(ctx), ch.ID); err != nil {
					return 0, fmt.Errorf("mark chapter %d pages read: %w", ch.ID, err)
				}
			}
			if err = r.updateReadProgress(ctx, tx, ch.ID, total, true, true); err != nil {
				return 0, err
			}
			count++
		}
		if err = tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit range read: %w", err)
		}
		return count, nil
	})
}

// MarkChaptersReadThrough marks every chapter with number <= maxNumber as
// read for the acting user, in one transaction (AniList pull-merge).
func (r *Repository) MarkChaptersReadThrough(ctx context.Context, titleID int64, maxNumber int) (int, error) {
	chapters, err := r.ListChapters(ctx, titleID)
	if err != nil {
		return 0, err
	}
	return retryBusy(ctx, func() (int, error) {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin read-through: %w", err)
		}
		defer func() {
			if err != nil {
				_ = tx.Rollback()
			}
		}()
		count := 0
		for _, ch := range chapters {
			if ch.NumberMain > maxNumber || ch.Read {
				continue
			}
			total := ch.TotalPages
			if total <= 0 {
				total = ch.Pages
			}
			if err = r.updateReadProgress(ctx, tx, ch.ID, total, true, true); err != nil {
				return 0, err
			}
			count++
		}
		if err = tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit read-through: %w", err)
		}
		return count, nil
	})
}

// MarkChapterRangeUnread clears read progress in an inclusive label/number range.
func (r *Repository) MarkChapterRangeUnread(ctx context.Context, titleID int64, from, to string) (int, error) {
	chapters, err := r.ListChapters(ctx, titleID)
	if err != nil {
		return 0, err
	}
	selected, err := selectChapterRange(chapters, from, to)
	if err != nil {
		return 0, err
	}
	if len(selected) == 0 {
		return 0, nil
	}
	ids := make([]any, 0, len(selected))
	marks := make([]string, 0, len(selected))
	for _, ch := range selected {
		ids = append(ids, ch.ID)
		marks = append(marks, "?")
	}
	in := "(" + strings.Join(marks, ",") + ")"
	return retryBusy(ctx, func() (int, error) {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin range unread: %w", err)
		}
		defer func() {
			if err != nil {
				_ = tx.Rollback()
			}
		}()
		if _, err = tx.ExecContext(ctx, `DELETE FROM chapter_read_pages WHERE user_id = ? AND chapter_id IN `+in, append([]any{auth.UserID(ctx)}, ids...)...); err != nil {
			return 0, fmt.Errorf("clear read pages: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM chapter_read_progress WHERE user_id = ? AND chapter_id IN `+in, append([]any{auth.UserID(ctx)}, ids...)...); err != nil {
			return 0, fmt.Errorf("clear read progress: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit range unread: %w", err)
		}
		return len(selected), nil
	})
}

// GetChapterReadStatus returns read progress for one chapter.
func (r *Repository) GetChapterReadStatus(ctx context.Context, chapterID int64) (ChapterReadStatus, error) {
	chapters, err := r.listReadChapters(ctx, `WHERE c.id = ?`, chapterID)
	if err != nil {
		return ChapterReadStatus{}, err
	}
	if len(chapters) == 0 {
		return ChapterReadStatus{}, fmt.Errorf("chapter %d not found", chapterID)
	}
	return chapters[0], nil
}

// TitlesByProvider maps a catalog provider's manga IDs to tracked title IDs,
// for marking search results that are already in the library.
func (r *Repository) TitlesByProvider(ctx context.Context, provider string) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT m.provider_id, t.id FROM titles t
		JOIN catalog_manga m ON m.id = t.catalog_manga_id
		WHERE m.provider = ?
	`, provider)
	if err != nil {
		return nil, fmt.Errorf("list titles by provider: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var providerID string
		var titleID int64
		if err := rows.Scan(&providerID, &titleID); err != nil {
			return nil, fmt.Errorf("scan provider title: %w", err)
		}
		out[providerID] = titleID
	}
	return out, rows.Err()
}

// FindByCatalog returns the tracked title for a catalog manga, if any.
func (r *Repository) FindByCatalog(ctx context.Context, catalogID int64) (Title, bool, error) {
	row := r.db.QueryRowContext(ctx, r.titleSelectQuery()+` WHERE t.catalog_manga_id = ? LIMIT 1`, auth.UserID(ctx), auth.UserID(ctx), auth.UserID(ctx), catalogID)
	title, err := scanTitle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Title{}, false, nil
	}
	if err != nil {
		return Title{}, false, fmt.Errorf("find title by catalog %d: %w", catalogID, err)
	}
	return title, true, nil
}

// GetChapterByLabel returns one discovered chapter by title and label.
func (r *Repository) GetChapterByLabel(ctx context.Context, titleID int64, label string) (Chapter, error) {
	row := r.db.QueryRowContext(ctx, chapterSelectQuery()+` WHERE c.title_id = ? AND c.label = ?`, titleID, label)
	chapter, err := scanChapter(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Chapter{}, fmt.Errorf("chapter %q not found", label)
		}
		return Chapter{}, fmt.Errorf("get chapter %q: %w", label, err)
	}

	return chapter, nil
}

// MarkDownloadStarted records that a chapter download started.
func (r *Repository) MarkDownloadStarted(ctx context.Context, chapterID int64) error {
	return r.markDownload(ctx, chapterID, "started", "", 0, 0, "")
}

// MarkDownloadCompleted records that a chapter download completed.
func (r *Repository) MarkDownloadCompleted(ctx context.Context, chapterID int64, outputFile string, bytes int64, pages int) error {
	return r.markDownload(ctx, chapterID, "completed", outputFile, bytes, pages, "")
}

// MarkDownloadFailed records that a chapter download failed.
func (r *Repository) MarkDownloadFailed(ctx context.Context, chapterID int64, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return r.markDownload(ctx, chapterID, "failed", "", 0, 0, msg)
}

// ReconcileStartedDownloads marks interrupted downloads as failed.
func (r *Repository) ReconcileStartedDownloads(ctx context.Context) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE downloads
		SET status = 'failed',
			error = 'download interrupted before completion',
			completed_at = NULL,
			updated_at = CURRENT_TIMESTAMP
		WHERE status = 'started'
	`)
	if err != nil {
		return 0, fmt.Errorf("reconcile started downloads: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count reconciled downloads: %w", err)
	}
	return count, nil
}

// ListCompletedDownloads returns completed downloads, optionally scoped to a title.
func (r *Repository) ListCompletedDownloads(ctx context.Context, titleID int64) ([]CompletedDownload, error) {
	return r.listDownloadsByStatus(ctx, titleID, `d.status = 'completed'`)
}

// ListScannableDownloads also includes failed rows whose files may exist
// again (moved by the Chapters/Volumes split, restored from backup, ...).
func (r *Repository) ListScannableDownloads(ctx context.Context, titleID int64) ([]CompletedDownload, error) {
	return r.listDownloadsByStatus(ctx, titleID, `d.status IN ('completed', 'failed')`)
}

func (r *Repository) listDownloadsByStatus(ctx context.Context, titleID int64, where string) ([]CompletedDownload, error) {
	q := `
		SELECT c.id, c.title_id, c.label, d.output_file
		FROM downloads d
		JOIN chapters c ON c.id = d.chapter_id
		WHERE ` + where + `
	`
	args := []any{}
	if titleID > 0 {
		q += ` AND c.title_id = ?`
		args = append(args, titleID)
	}
	q += ` ORDER BY c.title_id, c.number_main, c.suffix_type, c.suffix_num, c.label`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list completed downloads: %w", err)
	}
	defer rows.Close()

	var out []CompletedDownload
	for rows.Next() {
		var d CompletedDownload
		if err := rows.Scan(&d.ChapterID, &d.TitleID, &d.Label, &d.OutputFile); err != nil {
			return nil, fmt.Errorf("scan completed download: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completed downloads: %w", err)
	}

	return out, nil
}

func (r *Repository) markDownload(ctx context.Context, chapterID int64, status, outputFile string, bytes int64, pages int, msg string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO downloads (
			chapter_id,
			status,
			output_file,
			bytes,
			pages,
			attempts,
			error,
			started_at,
			completed_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, CASE WHEN ? = 'failed' THEN 1 ELSE 0 END, ?, CURRENT_TIMESTAMP, CASE WHEN ? = 'completed' THEN CURRENT_TIMESTAMP END, CURRENT_TIMESTAMP)
		ON CONFLICT(chapter_id) DO UPDATE SET
			status = excluded.status,
			output_file = CASE WHEN excluded.output_file = '' THEN downloads.output_file ELSE excluded.output_file END,
			bytes = excluded.bytes,
			pages = excluded.pages,
			attempts = CASE excluded.status
				WHEN 'failed' THEN downloads.attempts + 1
				WHEN 'completed' THEN 0
				ELSE downloads.attempts
			END,
			error = excluded.error,
			started_at = CASE WHEN excluded.status = 'started' THEN CURRENT_TIMESTAMP ELSE downloads.started_at END,
			completed_at = CASE WHEN excluded.status = 'completed' THEN CURRENT_TIMESTAMP ELSE NULL END,
			updated_at = CURRENT_TIMESTAMP
	`, chapterID, status, outputFile, bytes, pages, status, msg, status)
	if err != nil {
		return fmt.Errorf("mark download %s for chapter %d: %w", status, chapterID, err)
	}

	return nil
}

func (r *Repository) updateReadProgress(ctx context.Context, tx *sql.Tx, chapterID int64, totalPages int, forceComplete, manual bool) error {
	var readPages int
	var lastPage int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(page), 0)
		FROM chapter_read_pages
		WHERE user_id = ? AND chapter_id = ?
	`, auth.UserID(ctx), chapterID).Scan(&readPages, &lastPage); err != nil {
		return fmt.Errorf("count read pages: %w", err)
	}
	completed := forceComplete || (totalPages > 0 && readPages >= totalPages)
	completedInt := 0
	if completed {
		completedInt = 1
		if totalPages > 0 {
			readPages = totalPages
			lastPage = totalPages
		}
	}
	manualInt := 0
	if manual {
		manualInt = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chapter_read_progress (
			user_id,
			chapter_id,
			last_page,
			read_pages,
			total_pages,
			completed,
			manual,
			last_read_at,
			completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CASE WHEN ? = 1 THEN CURRENT_TIMESTAMP END)
		ON CONFLICT(user_id, chapter_id) DO UPDATE SET
			last_page = MAX(chapter_read_progress.last_page, excluded.last_page),
			read_pages = excluded.read_pages,
			total_pages = MAX(chapter_read_progress.total_pages, excluded.total_pages),
			completed = CASE WHEN excluded.completed = 1 THEN 1 ELSE chapter_read_progress.completed END,
			-- genuine page reading (manual=0) always wins over a prior bulk mark
			manual = MIN(chapter_read_progress.manual, excluded.manual),
			last_read_at = CURRENT_TIMESTAMP,
			completed_at = CASE
				WHEN excluded.completed = 1 THEN COALESCE(chapter_read_progress.completed_at, CURRENT_TIMESTAMP)
				ELSE chapter_read_progress.completed_at
			END
	`, auth.UserID(ctx), chapterID, lastPage, readPages, totalPages, completedInt, manualInt, completedInt); err != nil {
		return fmt.Errorf("update read progress: %w", err)
	}
	return nil
}

// ReadProgressIDs returns every chapter and volume id the user has progress
// rows for, so delta clients can prune local state deleted by unread marks.
func (r *Repository) ReadProgressIDs(ctx context.Context) (chapters, volumes []int64, err error) {
	scan := func(q string) ([]int64, error) {
		rows, err := r.db.QueryContext(ctx, q, auth.UserID(ctx))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []int64{}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				out = append(out, id)
			}
		}
		return out, rows.Err()
	}
	if chapters, err = scan(`SELECT chapter_id FROM chapter_read_progress WHERE user_id = ?`); err != nil {
		return nil, nil, err
	}
	volumes, err = scan(`SELECT volume_id FROM volume_read_progress WHERE user_id = ?`)
	return chapters, volumes, err
}

// ChaptersReadSince returns the user's chapter progress rows touched after since.
func (r *Repository) ChaptersReadSince(ctx context.Context, since string) ([]ChapterReadStatus, error) {
	return r.listReadChapters(ctx, `WHERE rp.last_read_at >= ?`, since)
}

func (r *Repository) listReadChapters(ctx context.Context, where string, args ...any) ([]ChapterReadStatus, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.title_id, c.label, c.title, c.url, c.number_main, c.suffix_type, c.suffix_num,
			c.volume_num, c.discovered_at, c.updated_at,
			COALESCE(d.status, ''), COALESCE(d.output_file, ''), COALESCE(d.bytes, 0), COALESCE(d.pages, 0),
			COALESCE(rp.last_page, 0), COALESCE(rp.read_pages, 0), COALESCE(NULLIF(rp.total_pages, 0), d.pages, 0),
			COALESCE(rp.completed, 0), COALESCE(rp.manual, 0), rp.last_read_at, rp.completed_at
		FROM chapters c
		LEFT JOIN downloads d ON d.chapter_id = c.id
		LEFT JOIN chapter_read_progress rp ON rp.chapter_id = c.id AND rp.user_id = ?
		`+where+`
		ORDER BY c.number_main, c.suffix_type, c.suffix_num, c.label
	`, append([]any{auth.UserID(ctx)}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("list read chapters: %w", err)
	}
	defer rows.Close()

	var out []ChapterReadStatus
	for rows.Next() {
		var cs ChapterReadStatus
		var discoveredAt, updatedAt, status string
		var completed, manual int
		var lastReadAt, completedAt sql.NullString
		if err := rows.Scan(&cs.ID, &cs.TitleID, &cs.Label, &cs.Title, &cs.URL, &cs.NumberMain,
			&cs.SuffixType, &cs.SuffixNum, &cs.VolumeNum, &discoveredAt, &updatedAt,
			&status, &cs.OutputFile, &cs.Bytes, &cs.Pages,
			&cs.LastPage, &cs.ReadPages, &cs.TotalPages, &completed, &manual, &lastReadAt, &completedAt); err != nil {
			return nil, fmt.Errorf("scan read chapter: %w", err)
		}
		cs.Downloaded = status == "completed"
		cs.Completed = completed != 0
		cs.Manual = manual != 0
		cs.DiscoveredAt, _ = database.ParseTime(discoveredAt)
		cs.UpdatedAt, _ = database.ParseTime(updatedAt)
		cs.LastReadAt = parseOptionalTime(lastReadAt)
		cs.CompletedAt = parseOptionalTime(completedAt)
		setFirstUnreadPage(&cs)
		out = append(out, cs)
	}
	return out, rows.Err()
}

func parseOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	t, err := database.ParseTime(value.String)
	if err != nil {
		return nil
	}
	return &t
}

func setFirstUnreadPage(chapter *ChapterReadStatus) {
	if chapter.Completed {
		chapter.FirstUnreadPage = 0
		return
	}
	if chapter.TotalPages > 0 {
		chapter.FirstUnreadPage = chapter.ReadPages + 1
		if chapter.FirstUnreadPage > chapter.TotalPages {
			chapter.FirstUnreadPage = chapter.TotalPages
		}
		return
	}
	chapter.FirstUnreadPage = chapter.LastPage + 1
	if chapter.FirstUnreadPage <= 0 {
		chapter.FirstUnreadPage = 1
	}
}

func hasDotDot(path string) bool {
	if path == "" {
		return false
	}
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == ".." {
			return true
		}
	}
	return false
}

func normalizeTitleParams(params AddTitleParams) AddTitleParams {
	params.SourceURL = strings.TrimSpace(params.SourceURL)
	params.SourceID = strings.TrimSpace(params.SourceID)
	params.DisplayTitle = strings.TrimSpace(params.DisplayTitle)
	params.OutputPath = strings.TrimSpace(params.OutputPath)
	params.RefreshInterval = strings.TrimSpace(params.RefreshInterval)

	if params.DisplayTitle == "" {
		params.DisplayTitle = params.SourceURL
	}

	return params
}

func chapterSelectQuery() string {
	return `
		SELECT
			c.id,
			c.title_id,
			c.label,
			c.title,
			c.url,
			c.number_main,
			c.suffix_type,
			c.suffix_num,
			c.volume_num,
			c.discovered_at,
			c.updated_at
		FROM chapters c
	`
}

func (r *Repository) titleSelectQuery() string {
	// Per-title aggregates are grouped once in a subquery (downloads and
	// read-progress are 1:1 per chapter, so no DISTINCT is needed) — far
	// cheaper than aggregating over the fanned-out join on large libraries.
	// missing_count must agree with ListMissingChapters (what a download job
	// will actually act on); chapters that failed past the attempt cap are
	// reported separately as failed_count.
	return fmt.Sprintf(`
		SELECT
			t.id,
			t.catalog_manga_id,
			COALESCE(t.source_id, ''),
			t.source_url,
			t.display_title,
			t.output_path,
			t.monitored,
			t.refresh_interval,
			t.last_refreshed_at,
			COALESCE(agg.discovered, 0) AS discovered_count,
			COALESCE(agg.completed, 0) AS completed_count,
			COALESCE(agg.missing, 0) AS missing_count,
			COALESCE(agg.failed, 0) AS failed_count,
			COALESCE(agg.read, 0) AS read_count,
			COALESCE(agg.bytes, 0) AS size_bytes,
			COALESCE(agg.pages, 0) AS pages,
			t.language_mode,
			t.language_gap,
			t.fetch_error,
			t.created_at,
			t.updated_at,
			COALESCE(m.cover_image, ''),
			COALESCE(m.status, ''),
			COALESCE(m.is_adult, 0),
			COALESCE(m.tags_json, '[]'),
			COALESCE(m.genres_json, '[]'),
			COALESCE(m.average_score, 0),
			uf.title_id IS NOT NULL,
			t.cover IS NOT NULL AND length(t.cover) > 0,
			COALESCE(vagg.count, 0),
			COALESCE(vagg.read, 0),
			COALESCE(vagg.bytes, 0),
			COALESCE(vagg.pages, 0)
		FROM titles t
		LEFT JOIN user_favourites uf ON uf.title_id = t.id AND uf.user_id = ?
		LEFT JOIN (
			SELECT v.title_id,
				COUNT(*) AS count,
				COUNT(CASE WHEN vr.completed = 1 THEN 1 END) AS read,
				SUM(v.bytes) AS bytes,
				SUM(v.pages) AS pages
			FROM volumes v
			LEFT JOIN volume_read_progress vr ON vr.volume_id = v.id AND vr.user_id = ?
			GROUP BY v.title_id
		) vagg ON vagg.title_id = t.id
		LEFT JOIN (
			SELECT
				c.title_id,
				COUNT(*) AS discovered,
				COUNT(CASE WHEN d.status = 'completed' THEN 1 END) AS completed,
				COUNT(CASE WHEN d.id IS NULL
					OR (d.status != 'completed' AND NOT (d.status = 'failed' AND d.attempts >= %d)) THEN 1 END) AS missing,
				COUNT(CASE WHEN d.status = 'failed' AND d.attempts >= %d THEN 1 END) AS failed,
				COUNT(CASE WHEN rp.completed = 1 THEN 1 END) AS read,
				SUM(CASE WHEN d.status = 'completed' THEN d.bytes ELSE 0 END) AS bytes,
				SUM(CASE WHEN d.status = 'completed' THEN d.pages ELSE 0 END) AS pages
			FROM chapters c
			LEFT JOIN downloads d ON d.chapter_id = c.id
			LEFT JOIN chapter_read_progress rp ON rp.chapter_id = c.id AND rp.user_id = ?
			GROUP BY c.title_id
		) agg ON agg.title_id = t.id
		LEFT JOIN catalog_manga m ON m.id = t.catalog_manga_id
	`, r.MaxDownloadAttempts, r.MaxDownloadAttempts)
}

func scanTitle(row database.Scanner) (Title, error) {
	var title Title
	var isAdult, favourite int
	var tagsJSON, genresJSON string
	var catalogID sql.NullInt64
	var monitored int
	var lastRefreshed sql.NullString
	var createdAt string
	var updatedAt string
	var customCover int

	if err := row.Scan(
		&title.ID,
		&catalogID,
		&title.SourceID,
		&title.SourceURL,
		&title.DisplayTitle,
		&title.OutputPath,
		&monitored,
		&title.RefreshInterval,
		&lastRefreshed,
		&title.DiscoveredCount,
		&title.CompletedCount,
		&title.MissingCount,
		&title.FailedCount,
		&title.ReadCount,
		&title.SizeBytes,
		&title.Pages,
		&title.LanguageMode,
		&title.LanguageGap,
		&title.FetchError,
		&createdAt,
		&updatedAt,
		&title.CoverImage,
		&title.ReleaseStatus,
		&isAdult,
		&tagsJSON,
		&genresJSON,
		&title.AverageScore,
		&favourite,
		&customCover,
		&title.VolumeCount,
		&title.VolumeReadCount,
		&title.VolumeBytes,
		&title.VolumePages,
	); err != nil {
		return Title{}, err
	}
	var tags, genres []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	_ = json.Unmarshal([]byte(genresJSON), &genres)
	title.ContentTags = append(tags, genres...)
	title.CustomCover = customCover != 0

	if catalogID.Valid {
		title.CatalogMangaID = &catalogID.Int64
	}
	title.Monitored = monitored != 0
	title.IsAdult = isAdult != 0
	title.Favourite = favourite != 0
	if lastRefreshed.Valid {
		t, err := database.ParseTime(lastRefreshed.String)
		if err != nil {
			return Title{}, err
		}
		title.LastRefreshedAt = &t
	}

	created, err := database.ParseTime(createdAt)
	if err != nil {
		return Title{}, err
	}
	updated, err := database.ParseTime(updatedAt)
	if err != nil {
		return Title{}, err
	}
	title.CreatedAt = created
	title.UpdatedAt = updated
	if title.CustomCover {
		title.CoverImage = fmt.Sprintf("/api/v1/covers/%d?v=%d", title.ID, title.UpdatedAt.Unix())
	}

	return title, nil
}

func scanChapter(row database.Scanner) (Chapter, error) {
	var chapter Chapter
	var discoveredAt string
	var updatedAt string

	if err := row.Scan(
		&chapter.ID,
		&chapter.TitleID,
		&chapter.Label,
		&chapter.Title,
		&chapter.URL,
		&chapter.NumberMain,
		&chapter.SuffixType,
		&chapter.SuffixNum,
		&chapter.VolumeNum,
		&discoveredAt,
		&updatedAt,
	); err != nil {
		return Chapter{}, err
	}

	discovered, err := database.ParseTime(discoveredAt)
	if err != nil {
		return Chapter{}, err
	}
	updated, err := database.ParseTime(updatedAt)
	if err != nil {
		return Chapter{}, err
	}
	chapter.DiscoveredAt = discovered
	chapter.UpdatedAt = updated

	return chapter, nil
}
