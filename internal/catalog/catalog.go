// Package catalog stores canonical manga metadata and source matches.
package catalog

import "time"

// Manga is canonical metadata for a wanted title.
type Manga struct {
	ID           int64             `json:"id"`
	Provider     string            `json:"provider"`
	ProviderID   string            `json:"provider_id"`
	TitleRomaji  string            `json:"title_romaji"`
	TitleEnglish string            `json:"title_english"`
	TitleNative  string            `json:"title_native"`
	Description  string            `json:"description"`
	CoverImage   string            `json:"cover_image"`
	BannerImage  string            `json:"banner_image,omitempty"`
	Status       string            `json:"status"`
	Format       string            `json:"format"`
	Chapters     *int              `json:"chapters,omitempty"`
	Volumes      *int              `json:"volumes,omitempty"`
	Synonyms     []string          `json:"synonyms,omitempty"`
	Genres       []string          `json:"genres,omitempty"`
	Authors      []string          `json:"authors,omitempty"`
	Year         int               `json:"year,omitempty"`
	AverageScore int               `json:"average_score,omitempty"`
	IsAdult      bool              `json:"is_adult"`
	Tags         []string          `json:"tags,omitempty"`
	Wanted       bool              `json:"wanted"`
	RawJSON      string            `json:"-"`
	UpdatedAt    time.Time         `json:"updated_at"`
	RelationType string            `json:"relation_type,omitempty"`
	ExternalIDs  map[string]string `json:"external_ids,omitempty"`
}

// Match is one possible source URL for a canonical manga.
type Match struct {
	ID                int64     `json:"id"`
	CatalogMangaID    int64     `json:"catalog_manga_id"`
	SourceID          string    `json:"source_id"`
	SourceURL         string    `json:"source_url"`
	Title             string    `json:"title"`
	Confidence        float64   `json:"confidence"`
	MatchMethod       string    `json:"match_method"`
	ChaptersFound     int       `json:"chapters_found"`
	SampleImagesFound int       `json:"sample_images_found"`
	Error             string    `json:"error,omitempty"`
	VerifiedAt        string    `json:"verified_at,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}
