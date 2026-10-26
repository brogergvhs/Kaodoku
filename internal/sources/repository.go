package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/database"
)

// Repository persists source profiles.
type Repository struct {
	db database.Handle
}

// NewRepository creates a source repository.
func NewRepository(db database.Handle) *Repository {
	return &Repository{db: db}
}

// Sync upserts profiles without clearing existing verification status.
func (r *Repository) Sync(ctx context.Context, profiles []Profile, origin string, prune bool) (err error) {
	origin = cleanOrigin(origin)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin source sync: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		p, err := normalizeProfile(profile)
		if err != nil {
			return err
		}
		ids = append(ids, p.ID)
		domains, err := encodeList(p.Domains)
		if err != nil {
			return fmt.Errorf("encode domains for %s: %w", p.ID, err)
		}
		extensions, err := encodeList(p.AllowedExtensions)
		if err != nil {
			return fmt.Errorf("encode extensions for %s: %w", p.ID, err)
		}
		languages, err := encodeList(p.Languages)
		if err != nil {
			return fmt.Errorf("encode languages for %s: %w", p.ID, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO sources (
				id,
				origin,
				name,
				domains_json,
				base_url,
				sample_manga_url,
				search_url,
				scraper,
				allowed_extensions_json,
				languages_json,
				min_chapters,
				requires_browser_solver,
				requires_browser_downloader,
				single_manga,
				chapterless,
				nsfw,
				enabled,
				profile_version,
				updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET
				origin = excluded.origin,
				name = excluded.name,
				domains_json = excluded.domains_json,
				base_url = excluded.base_url,
				sample_manga_url = excluded.sample_manga_url,
				search_url = excluded.search_url,
				scraper = excluded.scraper,
				allowed_extensions_json = excluded.allowed_extensions_json,
				languages_json = excluded.languages_json,
				min_chapters = excluded.min_chapters,
				requires_browser_solver = excluded.requires_browser_solver,
				requires_browser_downloader = excluded.requires_browser_downloader,
				single_manga = excluded.single_manga,
				chapterless = excluded.chapterless,
				nsfw = excluded.nsfw,
				enabled = sources.enabled,
				profile_version = excluded.profile_version,
				updated_at = CURRENT_TIMESTAMP
			WHERE sources.origin != 'local' OR excluded.origin = 'local'
		`, p.ID, origin, p.Name, domains, p.BaseURL, p.SampleMangaURL, p.SearchURL, p.Scraper, extensions, languages, p.MinChapters, database.BoolToInt(p.RequiresBrowserSolver), database.BoolToInt(p.RequiresBrowserDownload), database.BoolToInt(p.SingleManga), database.BoolToInt(p.Chapterless), database.BoolToInt(p.NSFW), database.BoolToInt(p.Enabled), p.Version)
		if err != nil {
			return fmt.Errorf("sync source %s: %w", p.ID, err)
		}
	}

	if prune && len(ids) > 0 {
		args := make([]any, 0, len(ids)+1)
		args = append(args, origin)
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		if _, err = tx.ExecContext(ctx,
			`DELETE FROM sources WHERE origin = ? AND id NOT IN (`+strings.Join(placeholders, ",")+`)`,
			args...); err != nil {
			return fmt.Errorf("prune %s sources: %w", origin, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit source sync: %w", err)
	}
	return nil
}

// SetEnabled toggles a source on or off. The choice is durable: built-in
// sync never overwrites enabled on existing rows.
func (r *Repository) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := r.db.ExecContext(ctx, `UPDATE sources SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, database.BoolToInt(enabled), id)
	if err != nil {
		return fmt.Errorf("set source %s enabled: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("source %s not found", id)
	}
	return nil
}

// RemoveLocal deletes a local source row.
func (r *Repository) RemoveLocal(ctx context.Context, id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	result, err := r.db.ExecContext(ctx, `DELETE FROM sources WHERE id = ? AND origin = 'local'`, id)
	if err != nil {
		return fmt.Errorf("remove local source %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check removed source %s: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("local source %q not found", id)
	}
	return nil
}

// List returns all known sources.
func (r *Repository) List(ctx context.Context) ([]Source, error) {
	rows, err := r.db.QueryContext(ctx, sourceSelect()+` ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}
	return out, nil
}

func (r *Repository) TitleCounts(ctx context.Context) (map[string]int, error) {
	return r.countsBySource(ctx, `
		SELECT source_id, COUNT(*) FROM (
			SELECT source_id, id AS title_id FROM titles WHERE COALESCE(source_id, '') <> ''
			UNION
			SELECT source_id, title_id FROM title_sources WHERE source_id <> ''
		) GROUP BY source_id`)
}

func (r *Repository) SourcesInUse(ctx context.Context) (map[string]int, error) {
	return r.countsBySource(ctx, `
		SELECT s.id, 1 FROM sources s
		WHERE EXISTS (SELECT 1 FROM titles t WHERE t.source_id = s.id)
			OR EXISTS (SELECT 1 FROM title_sources ts WHERE ts.source_id = s.id)`)
}

// FetchFailureCounts counts titles per source whose last chapter refresh failed.
func (r *Repository) FetchFailureCounts(ctx context.Context) (map[string]int, error) {
	return r.countsBySource(ctx, `
		SELECT source_id, COUNT(*) FROM titles
		WHERE fetch_error <> '' AND COALESCE(source_id, '') <> ''
		GROUP BY source_id`)
}

func (r *Repository) countsBySource(ctx context.Context, query string) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("count source titles: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan source title count: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source title counts: %w", err)
	}
	return out, nil
}

// Get returns one source by id.
func (r *Repository) Get(ctx context.Context, id string) (Source, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	row := r.db.QueryRowContext(ctx, sourceSelect()+` WHERE id = ?`, id)
	src, err := scanSource(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Source{}, fmt.Errorf("source %q not found", id)
		}
		return Source{}, fmt.Errorf("get source %q: %w", id, err)
	}
	return src, nil
}

// UpdateCheck stores the latest source verification result.
// SetFetchMethods overrides the learned chapter/image fetch methods for a
// source. Empty values mean "auto" (fall back to profile hints / re-learn).
func (r *Repository) SetFetchMethods(ctx context.Context, id, chapterFetch, imageFetch string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	result, err := r.db.ExecContext(ctx, `UPDATE sources SET chapter_fetch = ?, image_fetch = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, chapterFetch, imageFetch, id)
	if err != nil {
		return fmt.Errorf("set fetch methods for %s: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("source %q not found", id)
	}
	return nil
}

// CheckResult is the outcome of one verification run to persist.
type CheckResult struct {
	Status          string
	LastError       string
	ChaptersFound   int
	ImagesFound     int
	ImageExtensions []string
	Steps           []VerifyStep
	ChapterFetch    string
	ImageFetch      string
}

func (r *Repository) UpdateCheck(ctx context.Context, id string, res CheckResult) error {
	id = strings.ToLower(strings.TrimSpace(id))
	encoded, err := encodeList(res.ImageExtensions)
	if err != nil {
		return fmt.Errorf("encode image extensions: %w", err)
	}
	steps, err := json.Marshal(res.Steps)
	if err != nil {
		return fmt.Errorf("encode verify steps: %w", err)
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE sources SET
			status = ?,
			last_checked_at = CURRENT_TIMESTAMP,
			last_error = ?,
			chapters_found = ?,
			sample_images_found = ?,
			image_extensions_json = ?,
			verify_steps_json = ?,
			chapter_fetch = CASE WHEN ? = '' THEN chapter_fetch ELSE ? END,
			image_fetch = CASE WHEN ? = '' THEN image_fetch ELSE ? END,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, res.Status, res.LastError, res.ChaptersFound, res.ImagesFound, encoded, string(steps),
		res.ChapterFetch, res.ChapterFetch, res.ImageFetch, res.ImageFetch, id)
	if err != nil {
		return fmt.Errorf("update source check %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check source update %s: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("source %q not found", id)
	}
	return nil
}

func (r *Repository) UpdateHealth(ctx context.Context, id, status, lastError string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	result, err := r.db.ExecContext(ctx, `
		UPDATE sources SET
			status = ?,
			last_error = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, status, lastError, id)
	if err != nil {
		return fmt.Errorf("update source health %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check source health update %s: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("source %q not found", id)
	}
	return nil
}

func sourceSelect() string {
	return `
		SELECT
			id,
			origin,
			name,
			domains_json,
			base_url,
			sample_manga_url,
			search_url,
			scraper,
			allowed_extensions_json,
			COALESCE(languages_json, '[]'),
			min_chapters,
			requires_browser_solver,
			requires_browser_downloader,
			single_manga,
			COALESCE(chapterless, 0),
			COALESCE(nsfw, 0),
			enabled,
			profile_version,
			status,
			COALESCE(last_checked_at, ''),
			last_error,
			chapters_found,
			sample_images_found,
			image_extensions_json,
			verify_steps_json,
			chapter_fetch,
			image_fetch
		FROM sources`
}

func scanSource(scanner interface {
	Scan(dest ...any) error
}) (Source, error) {
	var src Source
	var domainsJSON, extensionsJSON, languagesJSON, imageExtensionsJSON, stepsJSON string
	var requiresBrowserSolver, requiresBrowserDownloader, singleManga, chapterless, nsfw, enabled int
	err := scanner.Scan(
		&src.ID,
		&src.Origin,
		&src.Name,
		&domainsJSON,
		&src.BaseURL,
		&src.SampleMangaURL,
		&src.SearchURL,
		&src.Scraper,
		&extensionsJSON,
		&languagesJSON,
		&src.MinChapters,
		&requiresBrowserSolver,
		&requiresBrowserDownloader,
		&singleManga,
		&chapterless,
		&nsfw,
		&enabled,
		&src.Version,
		&src.Status,
		&src.LastCheckedAt,
		&src.LastError,
		&src.ChaptersFound,
		&src.SampleImagesFound,
		&imageExtensionsJSON,
		&stepsJSON,
		&src.ChapterFetch,
		&src.ImageFetch,
	)
	if err != nil {
		return Source{}, err
	}
	if src.Domains, err = decodeList(domainsJSON); err != nil {
		return Source{}, fmt.Errorf("decode source domains: %w", err)
	}
	if src.AllowedExtensions, err = decodeList(extensionsJSON); err != nil {
		return Source{}, fmt.Errorf("decode source extensions: %w", err)
	}
	if src.Languages, err = decodeList(languagesJSON); err != nil {
		return Source{}, fmt.Errorf("decode source languages: %w", err)
	}
	if src.ImageExtensions, err = decodeList(imageExtensionsJSON); err != nil {
		return Source{}, fmt.Errorf("decode source image extensions: %w", err)
	}
	if stepsJSON != "" && stepsJSON != "[]" {
		if err := json.Unmarshal([]byte(stepsJSON), &src.VerifySteps); err != nil {
			return Source{}, fmt.Errorf("decode verify steps: %w", err)
		}
	}
	src.RequiresBrowserSolver = requiresBrowserSolver != 0
	src.RequiresBrowserDownload = requiresBrowserDownloader != 0
	src.SingleManga = singleManga != 0
	src.Chapterless = chapterless != 0
	src.NSFW = nsfw != 0
	src.Enabled = enabled != 0
	src.Origin = cleanOrigin(src.Origin)
	return src, nil
}

func cleanOrigin(origin string) string {
	switch origin {
	case OriginBuiltin, OriginRegistry, OriginLocal:
		return origin
	default:
		return OriginLocal
	}
}

func encodeList(values []string) (string, error) {
	data, err := json.Marshal(cleanStrings(values))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeList(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil, err
	}
	return out, nil
}
