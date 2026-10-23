package mangabaka

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Series struct {
	ID                                               int64
	IDs                                              map[string]string // tracker name -> id
	Type, Status, ContentRating                      string
	Title, English, Native, Romanized, Description   string
	Cover, StartDate, EndDate                        string
	Titles, Genres, Tags, Authors                    []string
	Year, Chapters, Volumes, Popularity, PrevPopular int
	Rating                                           float64
	UpdatedAt                                        time.Time
	Relation                                         string // set on related series
}

// CoverURL is the cover resized by MangaBaka's CDN to width px (150, 250 or 350).
func (s Series) CoverURL(width int) string {
	if s.Cover == "" {
		return ""
	}
	return fmt.Sprintf("https://cdn.mangabaka.dev/imgproxy/plain/x%d@1/%s", width, base64.RawURLEncoding.EncodeToString([]byte(s.Cover)))
}

const columns = `s.id, s.anilist, s.mal, s.kitsu, s.mangaupdates, s.type, s.status, s.content_rating, s.year, s.chapters, s.volumes,
	s.title, s.english, s.native, s.romanized, s.titles, s.description, s.cover, s.genres, s.tags, s.authors, s.rating,
	s.popularity, s.popularity_week, s.start_date, s.end_date, s.updated_at`

func scan(rows *sql.Rows, extra ...any) (Series, error) {
	var s Series
	var ids [4]sql.NullInt64
	var year, chapters, volumes, pop, prev sql.NullInt64
	var titles, genres, tags, authors, updated string
	err := rows.Scan(append([]any{&s.ID, &ids[0], &ids[1], &ids[2], &ids[3], &s.Type, &s.Status, &s.ContentRating, &year, &chapters, &volumes,
		&s.Title, &s.English, &s.Native, &s.Romanized, &titles, &s.Description, &s.Cover, &genres, &tags, &authors, &s.Rating,
		&pop, &prev, &s.StartDate, &s.EndDate, &updated}, extra...)...)
	s.IDs = map[string]string{}
	for i, id := range ids {
		if id.Valid {
			s.IDs[Trackers[i]] = strconv.FormatInt(id.Int64, 10)
		}
	}
	s.Year, s.Chapters, s.Volumes, s.Popularity, s.PrevPopular = int(year.Int64), int(chapters.Int64), int(volumes.Int64), int(pop.Int64), int(prev.Int64)
	_ = json.Unmarshal([]byte(titles), &s.Titles)
	_ = json.Unmarshal([]byte(genres), &s.Genres)
	_ = json.Unmarshal([]byte(tags), &s.Tags)
	_ = json.Unmarshal([]byte(authors), &s.Authors)
	s.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return s, err
}

// list scans series rows; an optional extra column after them lands in Relation.
func (s *Store) list(ctx context.Context, q string, args ...any) []Series {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []Series
	for rows.Next() {
		var extra sql.NullString
		var dest []any
		if len(cols) > 27 {
			dest = append(dest, &extra)
		}
		if sr, err := scan(rows, dest...); err == nil {
			sr.Relation = extra.String
			out = append(out, sr)
		}
	}
	return out
}

// ByProvider is the one active series carrying a tracker id; ids shared by several series match nothing.
func (s *Store) ByProvider(ctx context.Context, provider, id string) (Series, bool) {
	found := s.ByRefs(ctx, map[string][]string{provider: {id}})
	sr, ok := found[provider+":"+id]
	return sr, ok
}

// ByRefs looks up tracker ids in bulk, keyed "provider:id".
func (s *Store) ByRefs(ctx context.Context, refs map[string][]string) map[string]Series {
	out := map[string]Series{}
	for provider, ids := range refs {
		if !slices.Contains(Trackers, provider) {
			continue
		}
		var nums []any
		for _, id := range ids {
			if n, err := strconv.ParseInt(id, 10, 64); err == nil {
				nums = append(nums, n)
			}
		}
		for chunk := range slices.Chunk(nums, 500) {
			q := `SELECT ` + columns + ` FROM series s WHERE s.` + provider + ` IN (?` + strings.Repeat(", ?", len(chunk)-1) + `)
				AND NOT EXISTS (SELECT 1 FROM ambiguous a WHERE a.provider = '` + provider + `' AND a.provider_id = s.` + provider + `)`
			for _, sr := range s.list(ctx, q, chunk...) {
				out[provider+":"+sr.IDs[provider]] = sr
			}
		}
	}
	return out
}

// ByIDs follows merges, so an old id still finds its series.
func (s *Store) ByIDs(ctx context.Context, ids []int64) map[int64]Series {
	out := map[int64]Series{}
	for chunk := range slices.Chunk(ids, 500) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		q := `SELECT ` + columns + `, r.id FROM series r JOIN series s ON s.id = COALESCE(r.merged_into, r.id)
			WHERE r.id IN (?` + strings.Repeat(", ?", len(chunk)-1) + `) AND s.merged_into IS NULL`
		for _, sr := range s.list(ctx, q, args...) {
			asked, _ := strconv.ParseInt(sr.Relation, 10, 64)
			sr.Relation = ""
			out[asked] = sr
		}
	}
	return out
}

// Related lists a series' relations, closest first and without parodies, and its recommendations, strongest first.
func (s *Store) Related(ctx context.Context, id int64, limit int) (relations, recommendations []Series) {
	relations = s.list(ctx, `SELECT `+columns+`, r.type FROM relations r JOIN series s ON s.id = r.to_series_id
		WHERE r.series_id = ? AND r.type != 'parody' AND s.merged_into IS NULL
		ORDER BY r.type IN ('cameo', 'other', 'crossover'), s.popularity IS NULL, s.popularity LIMIT ?`, id, limit)
	recommendations = s.list(ctx, `SELECT `+columns+` FROM recommendations r JOIN series s ON s.id = r.rec_id
		WHERE r.series_id = ? AND s.merged_into IS NULL ORDER BY r.weight DESC LIMIT ?`, id, limit)
	return relations, recommendations
}

type Filter struct {
	GenreIn, GenreNotIn, TagIn, TagNotIn []string
	Sort                                 string // "", "popularity", "rating", "newest"
	Limit, Offset                        int
}

// Search matches every word against all known titles; an empty query browses by popularity.
func (s *Store) Search(ctx context.Context, query string, f Filter) ([]Series, bool) {
	from, order := "series s", "s.popularity IS NULL, s.popularity"
	var args []any
	if words := strings.Fields(query); len(words) > 0 {
		for i, w := range words {
			words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
		}
		from = `(SELECT series_id, MIN(rank) AS rank FROM titles WHERE titles MATCH ? GROUP BY series_id) m JOIN series s ON s.id = m.series_id`
		order = "m.rank, " + order
		args = append(args, strings.Join(words, " ")+"*")
	}
	q := `SELECT ` + columns + ` FROM ` + from + ` WHERE s.merged_into IS NULL`
	in := func(col string, names []string, not bool) {
		if len(names) == 0 {
			return
		}
		q += " AND "
		if not {
			q += "NOT "
		}
		q += `EXISTS (SELECT 1 FROM json_each(s.` + col + `) WHERE value IN (?` + strings.Repeat(", ?", len(names)-1) + `))`
		for _, n := range names {
			args = append(args, n)
		}
	}
	in("genres", f.GenreIn, false)
	in("genres", f.GenreNotIn, true)
	in("tags", f.TagIn, false)
	in("tags", f.TagNotIn, true)
	switch f.Sort {
	case "popularity":
		order = "s.popularity IS NULL, s.popularity"
	case "rating":
		order = "s.rating DESC"
	case "newest":
		order = "s.start_date DESC"
	}
	limit := max(f.Limit, 1)
	items := s.list(ctx, q+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, limit+1, f.Offset)...)
	if len(items) > limit {
		return items[:limit], true
	}
	return items, false
}

// Trending ranks the top 1000 series by how far they climbed in the last week, relative to their rank.
func (s *Store) Trending(ctx context.Context, limit int) []Series {
	return s.list(ctx, `SELECT `+columns+` FROM (SELECT * FROM series WHERE merged_into IS NULL AND popularity IS NOT NULL ORDER BY popularity LIMIT 1000) s
		ORDER BY (COALESCE(s.popularity_week, 2000) - s.popularity) * 1.0 / s.popularity DESC LIMIT ?`, limit)
}
