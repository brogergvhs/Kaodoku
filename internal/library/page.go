package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/database"
)

type TitleQuery struct {
	IDs           []int64
	UpdatedSince  string
	Search        string
	Name          string
	IncludeAny    [][]string
	Exclude       []string
	Monitored     string
	FavouriteOnly bool
	AdultOnly     bool
	NoAdult       bool
	Sources       []string
	Progress      string
	Content       string
	Downloaded    bool
	Sort, Dir     string
	Offset, Limit int
}

const titleFrom = ` FROM titles t LEFT JOIN catalog_manga m ON m.id = t.catalog_manga_id`

const tagMatch = `(EXISTS (SELECT 1 FROM json_each(COALESCE(m.tags_json, '[]')) WHERE casefold(trim(value)) IN (SELECT value FROM json_each(?)))
	OR EXISTS (SELECT 1 FROM json_each(COALESCE(m.genres_json, '[]')) WHERE casefold(trim(value)) IN (SELECT value FROM json_each(?))))`

func (r *Repository) missingChapter() string {
	return fmt.Sprintf(`(d.id IS NULL OR (d.status != 'completed' AND NOT (d.status = 'failed' AND d.attempts >= %d)))`, r.MaxDownloadAttempts)
}

func (r *Repository) titleWhere(q TitleQuery, uid int64) (string, []any) {
	where := []string{"1 = 1"}
	var args []any
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if q.IDs != nil {
		ids, _ := json.Marshal(q.IDs)
		add(`t.id IN (SELECT value FROM json_each(?))`, string(ids))
	}
	if q.UpdatedSince != "" {
		add(`t.updated_at >= ?`, q.UpdatedSince)
	}
	if q.Search != "" {
		add(`instr(casefold(t.display_title || ' ' || COALESCE(t.source_id, '') || ' ' || COALESCE(m.status, '')), ?) > 0`, strings.ToLower(q.Search))
	}
	if q.Name != "" {
		add(`instr(casefold(t.display_title), ?) > 0`, strings.ToLower(q.Name))
	}
	for _, group := range q.IncludeAny {
		if len(group) > 0 {
			tags := foldTags(group)
			add(tagMatch, tags, tags)
		}
	}
	if len(q.Exclude) > 0 {
		tags := foldTags(q.Exclude)
		add(`NOT `+tagMatch, tags, tags)
	}
	switch q.Monitored {
	case "on":
		add(`t.monitored = 1`)
	case "off":
		add(`t.monitored = 0`)
	}
	if q.FavouriteOnly {
		add(`EXISTS (SELECT 1 FROM user_favourites uf WHERE uf.title_id = t.id AND uf.user_id = ?)`, uid)
	}
	if q.AdultOnly {
		add(`COALESCE(m.is_adult, 0) = 1`)
	}
	if q.NoAdult {
		add(`COALESCE(m.is_adult, 0) = 0`)
	}
	if len(q.Sources) > 0 {
		var anyOf []string
		for _, s := range q.Sources {
			switch s {
			case "linked":
				anyOf = append(anyOf, `substr(t.source_url, 1, 4) = 'http'`)
			case "unlinked":
				anyOf = append(anyOf, `substr(t.source_url, 1, 4) != 'http'`)
			case "imported":
				anyOf = append(anyOf, `substr(t.source_url, 1, 6) = 'local:'`)
			default:
				anyOf = append(anyOf, `(COALESCE(t.source_id, '') = ? OR EXISTS (SELECT 1 FROM title_sources ts WHERE ts.title_id = t.id AND ts.source_id = ?))`)
				args = append(args, s, s)
			}
		}
		where = append(where, `(`+strings.Join(anyOf, ` OR `)+`)`)
	}
	hasChapters := `EXISTS (SELECT 1 FROM chapters c WHERE c.title_id = t.id)`
	hasMissing := `EXISTS (SELECT 1 FROM chapters c LEFT JOIN downloads d ON d.chapter_id = c.id WHERE c.title_id = t.id AND ` + r.missingChapter() + `)`
	hasVolumes := `EXISTS (SELECT 1 FROM volumes v WHERE v.title_id = t.id)`
	switch q.Progress {
	case "missing":
		add(hasMissing)
	case "complete":
		add(hasChapters + ` AND NOT ` + hasMissing)
	case "empty":
		add(`NOT ` + hasChapters)
	case "needs-download":
		add(`(` + hasMissing + ` OR NOT ` + hasChapters + `)`)
	}
	if q.Downloaded {
		add(`EXISTS (SELECT 1 FROM chapters c JOIN downloads d ON d.chapter_id = c.id WHERE c.title_id = t.id AND d.status = 'completed')`)
	}
	switch q.Content {
	case "chapters":
		add(hasChapters)
	case "no-chapters":
		add(`NOT ` + hasChapters)
	case "volumes":
		add(hasVolumes)
	case "no-volumes":
		add(`NOT ` + hasVolumes)
	}
	return strings.Join(where, ` AND `), args
}

func (r *Repository) titleOrder(q TitleQuery) string {
	chapterBytes := `(SELECT COALESCE(SUM(d.bytes), 0) FROM chapters c JOIN downloads d ON d.chapter_id = c.id WHERE c.title_id = t.id AND d.status = 'completed')`
	volumeBytes := `(SELECT COALESCE(SUM(v.bytes), 0) FROM volumes v WHERE v.title_id = t.id)`
	key := map[string]string{
		"title":         `t.display_title COLLATE NOCASE`,
		"missing":       `(SELECT COUNT(*) FROM chapters c LEFT JOIN downloads d ON d.chapter_id = c.id WHERE c.title_id = t.id AND ` + r.missingChapter() + `)`,
		"failed":        fmt.Sprintf(`(SELECT COUNT(*) FROM chapters c JOIN downloads d ON d.chapter_id = c.id WHERE c.title_id = t.id AND d.status = 'failed' AND d.attempts >= %d)`, r.MaxDownloadAttempts),
		"chapters":      `(SELECT COUNT(*) FROM chapters c WHERE c.title_id = t.id)`,
		"volumes":       `(SELECT COUNT(*) FROM volumes v WHERE v.title_id = t.id)`,
		"size":          chapterBytes + ` + ` + volumeBytes,
		"size-chapters": chapterBytes,
		"size-volumes":  volumeBytes,
		"rating":        `COALESCE(m.average_score, 0)`,
		"updated":       `t.updated_at`,
		"added":         `t.created_at`,
		"status":        `COALESCE(m.status, '')`,
	}[q.Sort]
	order := `t.display_title COLLATE NOCASE, t.id`
	dir := ``
	if q.Dir == "desc" {
		dir = ` DESC`
	}
	switch q.Sort {
	case "":
		return order
	case "title", "added", "updated":
		return key + dir + `, t.id` + dir
	}
	return key + dir + `, ` + order
}

func (r *Repository) CountTitles(ctx context.Context, q TitleQuery) (int, error) {
	where, args := r.titleWhere(q, auth.UserID(ctx))
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*)`+titleFrom+` WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count titles: %w", err)
	}
	return n, nil
}

func (r *Repository) TitleIDs(ctx context.Context, q TitleQuery) ([]int64, error) {
	q.Offset, q.Limit = 0, 0
	return r.pageIDs(ctx, q)
}

func (r *Repository) PageTitles(ctx context.Context, q TitleQuery) ([]Title, error) {
	ids, err := r.pageIDs(ctx, q)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return r.TitlesByIDs(ctx, ids)
}

func (r *Repository) pageIDs(ctx context.Context, q TitleQuery) ([]int64, error) {
	where, args := r.titleWhere(q, auth.UserID(ctx))
	query := `SELECT t.id` + titleFrom + ` WHERE ` + where + ` ORDER BY ` + r.titleOrder(q)
	if q.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d OFFSET %d`, q.Limit, max(q.Offset, 0))
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("page titles: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TitleBasics returns matching titles by name with only their own columns set, no per-chapter aggregates.
func (r *Repository) TitleBasics(ctx context.Context, q TitleQuery) ([]Title, error) {
	where, args := r.titleWhere(q, auth.UserID(ctx))
	rows, err := r.db.QueryContext(ctx, `SELECT t.id, t.catalog_manga_id, COALESCE(t.source_id, ''), t.source_url, t.display_title, t.output_path, t.monitored, t.refresh_interval, t.last_refreshed_at`+titleFrom+` WHERE `+where+` ORDER BY t.display_title COLLATE NOCASE, t.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list title basics: %w", err)
	}
	defer rows.Close()
	var out []Title
	for rows.Next() {
		var t Title
		var catalogID sql.NullInt64
		var lastRefreshed sql.NullString
		if err := rows.Scan(&t.ID, &catalogID, &t.SourceID, &t.SourceURL, &t.DisplayTitle, &t.OutputPath, &t.Monitored, &t.RefreshInterval, &lastRefreshed); err != nil {
			return nil, err
		}
		if catalogID.Valid {
			t.CatalogMangaID = &catalogID.Int64
		}
		if lastRefreshed.Valid {
			at, err := database.ParseTime(lastRefreshed.String)
			if err != nil {
				return nil, err
			}
			t.LastRefreshedAt = &at
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) TitlesByIDs(ctx context.Context, ids []int64) ([]Title, error) {
	raw, _ := json.Marshal(ids)
	list, err := r.titlesIn(ctx, `SELECT value FROM json_each(?)`, "", string(raw))
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Title, len(list))
	for _, t := range list {
		byID[t.ID] = t
	}
	out := make([]Title, 0, len(ids))
	for _, id := range ids {
		if t, ok := byID[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func foldTags(tags []string) string {
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = strings.ToLower(strings.TrimSpace(t))
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func (r *Repository) TopGenre(ctx context.Context, q TitleQuery) (string, error) {
	uid := auth.UserID(ctx)
	where, args := r.titleWhere(q, uid)
	var genre string
	err := r.db.QueryRowContext(ctx, `SELECT g.value`+titleFrom+`
		JOIN json_each(COALESCE(m.genres_json, '[]')) g
		JOIN (SELECT title_id, SUM(n) AS n FROM (
			SELECT c.title_id AS title_id, COUNT(*) AS n FROM chapter_read_progress rp JOIN chapters c ON c.id = rp.chapter_id
			WHERE rp.user_id = ? AND rp.completed = 1 GROUP BY c.title_id
			UNION ALL
			SELECT v.title_id, COUNT(*) FROM volume_read_progress vr JOIN volumes v ON v.id = vr.volume_id
			WHERE vr.user_id = ? AND vr.completed = 1 GROUP BY v.title_id
		) GROUP BY title_id) reads ON reads.title_id = t.id
		WHERE `+where+`
		GROUP BY g.value ORDER BY SUM(reads.n) DESC, g.value LIMIT 1`, append([]any{uid, uid}, args...)...).Scan(&genre)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return genre, err
}

func (r *Repository) TitleNames(ctx context.Context, q TitleQuery) ([]string, error) {
	where, args := r.titleWhere(q, auth.UserID(ctx))
	rows, err := r.db.QueryContext(ctx, `SELECT t.display_title`+titleFrom+` WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list title names: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (r *Repository) ReadTitleIDs(ctx context.Context) ([]int64, error) {
	uid := auth.UserID(ctx)
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.title_id FROM chapter_read_progress rp JOIN chapters c ON c.id = rp.chapter_id WHERE rp.user_id = ? AND rp.completed = 1
		UNION
		SELECT v.title_id FROM volume_read_progress vr JOIN volumes v ON v.id = vr.volume_id WHERE vr.user_id = ? AND vr.completed = 1`, uid, uid)
	if err != nil {
		return nil, fmt.Errorf("list read titles: %w", err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *Repository) FailingTitles(ctx context.Context, sourceID string) ([]Title, error) {
	return r.titlesIn(ctx, `SELECT id FROM titles WHERE source_id = ? AND fetch_error != ''`, ` ORDER BY t.display_title COLLATE NOCASE, t.id`, sourceID)
}
