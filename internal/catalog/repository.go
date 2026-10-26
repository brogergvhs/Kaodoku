package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/database"
)

// Repository persists canonical manga and source matches.
type Repository struct {
	db database.Handle
}

// NewRepository creates a catalog repository.
func NewRepository(db database.Handle) *Repository {
	return &Repository{db: db}
}

// UpsertManga stores canonical metadata.
func (r *Repository) UpsertManga(ctx context.Context, m Manga) (Manga, error) {
	m.Provider = strings.TrimSpace(m.Provider)
	m.ProviderID = strings.TrimSpace(m.ProviderID)
	if m.Provider == "" || m.ProviderID == "" {
		return Manga{}, errors.New("provider and provider_id are required")
	}
	synonyms, err := json.Marshal(cleanStrings(m.Synonyms))
	if err != nil {
		return Manga{}, fmt.Errorf("encode synonyms: %w", err)
	}
	genres, err := json.Marshal(cleanStrings(m.Genres))
	if err != nil {
		return Manga{}, err
	}
	tags, err := json.Marshal(cleanStrings(m.Tags))
	if err != nil {
		return Manga{}, fmt.Errorf("encode genres: %w", err)
	}
	authors, err := json.Marshal(m.Authors)
	if err != nil {
		return Manga{}, fmt.Errorf("encode authors: %w", err)
	}
	var chapters, volumes any
	if m.Chapters != nil {
		chapters = *m.Chapters
	}
	if m.Volumes != nil {
		volumes = *m.Volumes
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO catalog_manga (
			provider, provider_id, title_romaji, title_english, title_native,
			description, cover_image, banner_image, status, format, chapters, volumes, synonyms_json,
			genres_json, authors_json, year, average_score, is_adult, tags_json, wanted, raw_json, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(provider, provider_id) DO UPDATE SET
			title_romaji = excluded.title_romaji,
			title_english = excluded.title_english,
			title_native = excluded.title_native,
			description = excluded.description,
			cover_image = excluded.cover_image,
			banner_image = excluded.banner_image,
			status = excluded.status,
			format = excluded.format,
			chapters = excluded.chapters,
			volumes = excluded.volumes,
			synonyms_json = excluded.synonyms_json,
			genres_json = excluded.genres_json,
			authors_json = excluded.authors_json,
			year = excluded.year,
			average_score = excluded.average_score,
			is_adult = excluded.is_adult,
			tags_json = excluded.tags_json,
			wanted = CASE WHEN excluded.wanted != 0 THEN excluded.wanted ELSE catalog_manga.wanted END,
			raw_json = excluded.raw_json,
			updated_at = CURRENT_TIMESTAMP
		RETURNING id
	`, m.Provider, m.ProviderID, m.TitleRomaji, m.TitleEnglish, m.TitleNative, m.Description,
		m.CoverImage, m.BannerImage, m.Status, m.Format, chapters, volumes, string(synonyms),
		string(genres), string(authors), m.Year, m.AverageScore, database.BoolToInt(m.IsAdult), string(tags), database.BoolToInt(m.Wanted), m.RawJSON)
	var id int64
	if err := row.Scan(&id); err != nil {
		return Manga{}, fmt.Errorf("upsert manga: %w", err)
	}
	for p, pid := range m.ExternalIDs {
		if p != m.Provider && pid != "" {
			if err := r.SetExternalID(ctx, id, ExternalID{Provider: p, ProviderID: pid, Method: "cross_link", Confirmed: true}); err != nil {
				return Manga{}, err
			}
		}
	}
	return r.GetManga(ctx, id)
}

// ExternalID Method is cross_link, title_match, mangabaka, user, or miss (searched, nothing found).
type ExternalID struct {
	Provider   string
	ProviderID string
	Method     string
	Confirmed  bool
	CheckedAt  string
}

// SetExternalID stores a mapping; a user-made one is only replaced by another, and a MangaBaka one never replaces a confirmed link.
func (r *Repository) SetExternalID(ctx context.Context, catalogID int64, e ExternalID) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO catalog_external_ids (catalog_manga_id, provider, provider_id, method, confirmed, checked_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(catalog_manga_id, provider) DO UPDATE SET
			provider_id = excluded.provider_id, method = excluded.method,
			confirmed = excluded.confirmed, checked_at = CURRENT_TIMESTAMP
		WHERE (catalog_external_ids.method != 'user' OR excluded.method = 'user')
			AND NOT (excluded.method = 'mangabaka' AND catalog_external_ids.confirmed = 1 AND catalog_external_ids.method != 'mangabaka')`,
		catalogID, e.Provider, e.ProviderID, e.Method, database.BoolToInt(e.Confirmed))
	return err
}

func (r *Repository) ExternalIDs(ctx context.Context, catalogID int64) ([]ExternalID, error) {
	return database.Collect(ctx, r.db, func(row database.Scanner) (e ExternalID, err error) {
		return e, row.Scan(&e.Provider, &e.ProviderID, &e.Method, &e.Confirmed, &e.CheckedAt)
	}, `SELECT provider, provider_id, method, confirmed, checked_at FROM catalog_external_ids WHERE catalog_manga_id = ? ORDER BY provider`, catalogID)
}

func (r *Repository) CopyExternalIDs(ctx context.Context, fromID, toID int64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO catalog_external_ids (catalog_manga_id, provider, provider_id, method, confirmed, checked_at)
		SELECT ?, provider, provider_id, method, confirmed, checked_at FROM catalog_external_ids WHERE catalog_manga_id = ?`, toID, fromID)
	return err
}

// DeleteMatches drops all cached source decisions for a manga, forcing the
// next match run to search fresh.
func (r *Repository) DeleteMatches(ctx context.Context, catalogMangaID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM title_source_matches WHERE catalog_manga_id = ?`, catalogMangaID)
	return err
}

// Relation is a provider relation to another manga, kept for grouping
// connected titles into collections.
type Relation struct {
	ProviderID string
	Type       string
}

// CollectionRelation reports whether a MediaRelation type links entries that
// belong to the same work/collection (vs cross-media or incidental links).
func CollectionRelation(t string) bool {
	switch t {
	case "SEQUEL", "PREQUEL", "PARENT", "SIDE_STORY", "ALTERNATIVE", "SPIN_OFF", "SUMMARY", "COMPILATION", "CONTAINS":
		return true
	}
	return false
}

// ReplaceRelations swaps the stored collection relations for one media.
func (r *Repository) ReplaceRelations(ctx context.Context, fromID string, rels []Relation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_relations WHERE from_id = ?`, fromID); err != nil {
		return err
	}
	for _, rel := range rels {
		if rel.ProviderID == "" || rel.ProviderID == fromID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO catalog_relations (from_id, to_id, relation) VALUES (?, ?, ?)`, fromID, rel.ProviderID, rel.Type); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CollectionEdges returns every stored relation as an (from, to) provider-id
// pair for building the collection graph.
func (r *Repository) CollectionEdges(ctx context.Context) ([][2]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT from_id, to_id FROM catalog_relations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, b})
	}
	return out, rows.Err()
}

// ContentTag is one provider genre or tag name, for filters and content-guard pickers.
type ContentTag struct {
	Name        string
	Kind        string // "genre" or "tag"
	Category    string // tag group, e.g. AniList's "Theme-Fantasy"; empty for genres
	Description string
	Slug        string // provider filter key when it differs from Name
	IsAdult     bool
}

// ReplaceContentTags swaps one provider's stored vocabulary.
func (r *Repository) ReplaceContentTags(ctx context.Context, provider string, tags []ContentTag) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_tags WHERE provider = ?`, provider); err != nil {
		return err
	}
	for _, t := range tags {
		if name := strings.TrimSpace(t.Name); name != "" {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO provider_tags (provider, name, kind, category, description, slug, is_adult) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				provider, name, cmp.Or(t.Kind, "tag"), t.Category, t.Description, t.Slug, t.IsAdult || AdultName(name)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ContentTags merges every provider's vocabulary by name; a name adult anywhere stays adult.
func (r *Repository) ContentTags(ctx context.Context) ([]ContentTag, error) {
	return r.contentTags(ctx, `SELECT name, MIN(kind), MAX(category), MAX(description), '', MAX(is_adult) FROM provider_tags
		GROUP BY name COLLATE NOCASE ORDER BY MIN(kind) = 'genre' DESC, MAX(category) COLLATE NOCASE, name COLLATE NOCASE`)
}

// ProviderTags lists one provider's vocabulary, genres first.
func (r *Repository) ProviderTags(ctx context.Context, provider string) ([]ContentTag, error) {
	return r.contentTags(ctx, `SELECT name, kind, category, description, slug, is_adult FROM provider_tags WHERE provider = ?
		ORDER BY kind = 'genre' DESC, category COLLATE NOCASE, name COLLATE NOCASE`, provider)
}

func (r *Repository) HasProviderTags(ctx context.Context, provider string) bool {
	var one int
	return r.db.QueryRowContext(ctx, `SELECT 1 FROM provider_tags WHERE provider = ? LIMIT 1`, provider).Scan(&one) == nil
}

func (r *Repository) contentTags(ctx context.Context, query string, args ...any) ([]ContentTag, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentTag
	for rows.Next() {
		var t ContentTag
		if err := rows.Scan(&t.Name, &t.Kind, &t.Category, &t.Description, &t.Slug, &t.IsAdult); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetManga returns one canonical manga row.
// MangaByIDs returns the given manga keyed by ID, in one query.
func (r *Repository) MangaByIDs(ctx context.Context, ids []int64) (map[int64]Manga, error) {
	out := make(map[int64]Manga, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	rows, err := r.db.QueryContext(ctx, mangaSelect()+` WHERE id IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("load manga batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanManga(rows)
		if err != nil {
			return nil, err
		}
		out[m.ID] = m
	}
	return out, rows.Err()
}

// MangaAuthors loads only ID, Provider, ProviderID and Authors for the given manga.
func (r *Repository) MangaAuthors(ctx context.Context, ids []int64) (map[int64]Manga, error) {
	raw, _ := json.Marshal(ids)
	out := make(map[int64]Manga, len(ids))
	err := database.Each(ctx, r.db, `SELECT id, provider, provider_id, authors_json FROM catalog_manga WHERE id IN (SELECT value FROM json_each(?))`, []any{string(raw)}, func(row database.Scanner) error {
		var m Manga
		var authors string
		if err := row.Scan(&m.ID, &m.Provider, &m.ProviderID, &authors); err != nil {
			return err
		}
		err := json.Unmarshal([]byte(authors), &m.Authors)
		out[m.ID] = m
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("load manga authors: %w", err)
	}
	return out, nil
}

func (r *Repository) GetManga(ctx context.Context, id int64) (Manga, error) {
	row := r.db.QueryRowContext(ctx, mangaSelect()+` WHERE id = ?`, id)
	m, err := scanManga(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Manga{}, fmt.Errorf("catalog manga %d not found", id)
		}
		return Manga{}, fmt.Errorf("get manga: %w", err)
	}
	return m, nil
}

// MangaByProvider returns a cached manga by its provider id, or ok=false if it
// has not been fetched into the local catalog yet.
func (r *Repository) MangaByProvider(ctx context.Context, provider, providerID string) (Manga, bool, error) {
	row := r.db.QueryRowContext(ctx, mangaSelect()+` WHERE provider = ? AND provider_id = ?`, provider, providerID)
	m, err := scanManga(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Manga{}, false, nil
		}
		return Manga{}, false, fmt.Errorf("get manga by provider: %w", err)
	}
	return m, true, nil
}

// MangaByProviderIDs returns stored manga of one provider keyed by provider id, in one query.
func (r *Repository) MangaByProviderIDs(ctx context.Context, provider string, ids []string) (map[string]Manga, error) {
	raw, _ := json.Marshal(ids)
	list, err := database.Collect(ctx, r.db, scanManga, mangaSelect()+` WHERE provider = ? AND provider_id IN (SELECT value FROM json_each(?))`, provider, string(raw))
	if err != nil {
		return nil, fmt.Errorf("load manga by provider ids: %w", err)
	}
	out := make(map[string]Manga, len(list))
	for _, m := range list {
		out[m.ProviderID] = m
	}
	return out, nil
}

// ListWanted returns wanted canonical titles.
func (r *Repository) ListWanted(ctx context.Context) ([]Manga, error) {
	rows, err := r.db.QueryContext(ctx, mangaSelect()+` WHERE wanted = 1 ORDER BY title_english COLLATE NOCASE, title_romaji COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("list wanted: %w", err)
	}
	defer rows.Close()
	var out []Manga
	for rows.Next() {
		m, err := scanManga(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordMatchMiss marks that a source yielded no match for a title, so it is
// skipped on the next attempt until the cache expires. The synthetic URL keeps
// the marker distinct from real matches and out of the public match list.
func (r *Repository) RecordMatchMiss(ctx context.Context, catalogMangaID int64, sourceID string) error {
	_, err := r.UpsertMatch(ctx, Match{
		CatalogMangaID: catalogMangaID,
		SourceID:       sourceID,
		SourceURL:      "miss:" + sourceID,
		MatchMethod:    "miss",
		Error:          "no match",
	})
	return err
}

// ClearMissesForSource drops cached no-match markers so a source that just
// turned usable is retried immediately instead of after the 24h cache.
func (r *Repository) ClearMissesForSource(ctx context.Context, sourceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM title_source_matches WHERE source_id = ? AND match_method = 'miss'`, sourceID)
	return err
}

func (r *Repository) MoveMatches(ctx context.Context, fromID, toID int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE OR IGNORE title_source_matches SET catalog_manga_id = ? WHERE catalog_manga_id = ?`, toID, fromID)
	return err
}

// UpsertMatch stores a source match.
func (r *Repository) UpsertMatch(ctx context.Context, m Match) (Match, error) {
	m.SourceID = strings.TrimSpace(m.SourceID)
	m.SourceURL = strings.TrimSpace(m.SourceURL)
	if m.CatalogMangaID <= 0 || m.SourceURL == "" {
		return Match{}, errors.New("catalog_manga_id and source_url are required")
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO title_source_matches (
			catalog_manga_id, source_id, source_url, title, confidence,
			match_method, chapters_found, sample_images_found, error,
			verified_at, updated_at
		) VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(catalog_manga_id, source_url) DO UPDATE SET
			source_id = excluded.source_id,
			title = excluded.title,
			confidence = excluded.confidence,
			match_method = excluded.match_method,
			chapters_found = excluded.chapters_found,
			sample_images_found = excluded.sample_images_found,
			error = excluded.error,
			verified_at = excluded.verified_at,
			updated_at = CURRENT_TIMESTAMP
		RETURNING id
	`, m.CatalogMangaID, m.SourceID, m.SourceURL, m.Title, m.Confidence, nonEmpty(m.MatchMethod, "slug_probe"), m.ChaptersFound, m.SampleImagesFound, m.Error)
	var id int64
	if err := row.Scan(&id); err != nil {
		return Match{}, fmt.Errorf("upsert match: %w", err)
	}
	return r.GetMatch(ctx, id)
}

// GetMatch returns one source match.
func (r *Repository) GetMatch(ctx context.Context, id int64) (Match, error) {
	row := r.db.QueryRowContext(ctx, matchSelect()+` WHERE id = ?`, id)
	m, err := scanMatch(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Match{}, fmt.Errorf("source match %d not found", id)
		}
		return Match{}, fmt.Errorf("get source match: %w", err)
	}
	return m, nil
}

// ListMatches returns matches for one canonical title.
func (r *Repository) ListMatches(ctx context.Context, catalogMangaID int64) ([]Match, error) {
	rows, err := r.db.QueryContext(ctx, matchSelect()+`
		WHERE catalog_manga_id = ?
		ORDER BY chapters_found DESC, confidence DESC, source_id COLLATE NOCASE
	`, catalogMangaID)
	if err != nil {
		return nil, fmt.Errorf("list source matches: %w", err)
	}
	defer rows.Close()
	var out []Match
	for rows.Next() {
		m, err := scanMatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func mangaSelect() string {
	return `
		SELECT id, provider, provider_id, title_romaji, title_english, title_native,
			description, cover_image, banner_image, status, format, chapters, volumes, synonyms_json,
			genres_json, authors_json, year, average_score, COALESCE(is_adult, 0), COALESCE(tags_json, '[]'), wanted, raw_json, updated_at
		FROM catalog_manga`
}

func matchSelect() string {
	return `
		SELECT id, catalog_manga_id, COALESCE(source_id, ''), source_url, title,
			confidence, match_method, chapters_found, sample_images_found,
			error, COALESCE(verified_at, ''), updated_at
		FROM title_source_matches`
}

func scanManga(row database.Scanner) (Manga, error) {
	var m Manga
	var chapters, volumes sql.NullInt64
	var synonymsJSON, genresJSON, authorsJSON, tagsJSON string
	var wanted, isAdult int
	var updated string
	if err := row.Scan(&m.ID, &m.Provider, &m.ProviderID, &m.TitleRomaji, &m.TitleEnglish,
		&m.TitleNative, &m.Description, &m.CoverImage, &m.BannerImage, &m.Status, &m.Format, &chapters, &volumes,
		&synonymsJSON, &genresJSON, &authorsJSON, &m.Year, &m.AverageScore, &isAdult, &tagsJSON, &wanted, &m.RawJSON, &updated); err != nil {
		return Manga{}, err
	}
	m.IsAdult = isAdult != 0
	if chapters.Valid {
		v := int(chapters.Int64)
		m.Chapters = &v
	}
	if volumes.Valid {
		v := int(volumes.Int64)
		m.Volumes = &v
	}
	for _, f := range []struct {
		name string
		raw  string
		dst  *[]string
	}{
		{"synonyms", synonymsJSON, &m.Synonyms},
		{"genres", genresJSON, &m.Genres},
		{"authors", authorsJSON, &m.Authors},
		{"tags", tagsJSON, &m.Tags},
	} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return Manga{}, fmt.Errorf("decode %s for manga %d: %w", f.name, m.ID, err)
		}
	}
	m.Wanted = wanted != 0
	t, err := database.ParseTime(updated)
	if err != nil {
		return Manga{}, err
	}
	m.UpdatedAt = t
	return m, nil
}

func scanMatch(row database.Scanner) (Match, error) {
	var m Match
	var updated string
	if err := row.Scan(&m.ID, &m.CatalogMangaID, &m.SourceID, &m.SourceURL, &m.Title,
		&m.Confidence, &m.MatchMethod, &m.ChaptersFound, &m.SampleImagesFound,
		&m.Error, &m.VerifiedAt, &updated); err != nil {
		return Match{}, err
	}
	t, err := database.ParseTime(updated)
	if err != nil {
		return Match{}, err
	}
	m.UpdatedAt = t
	return m, nil
}

func cleanStrings(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
	}
	return out
}

func nonEmpty(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
