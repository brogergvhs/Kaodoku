// Package sources stores scraper source profiles and verification results.
package sources

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/providers/registry"
)

const (
	StatusHealthy    = "healthy"
	StatusDegraded   = "degraded"
	StatusRequiresCF = "requires_cf"
	StatusBroken     = "broken"

	// Learned fetch methods. Chapter/HTML uses http or the CF solver;
	// images use http or the browser (Selenium) downloader.
	FetchHTTP    = "http"
	FetchSolver  = "solver"
	FetchBrowser = "browser"

	OriginBuiltin  = "builtin"
	OriginRegistry = "registry"
	OriginLocal    = "local"
)

// Profile describes a supported manga website.
type Profile struct {
	ID                      string   `json:"id" yaml:"id"`
	Name                    string   `json:"name" yaml:"name"`
	Domains                 []string `json:"domains" yaml:"domains"`
	BaseURL                 string   `json:"base_url" yaml:"base_url"`
	SampleMangaURL          string   `json:"sample_manga_url" yaml:"sample_manga_url"`
	SearchURL               string   `json:"search_url" yaml:"search_url"`
	Scraper                 string   `json:"scraper" yaml:"scraper"`
	AllowedExtensions       []string `json:"allowed_extensions" yaml:"allowed_extensions"`
	Languages               []string `json:"languages,omitempty" yaml:"languages,omitempty"`
	MinChapters             int      `json:"min_chapters" yaml:"min_chapters"`
	RequiresBrowserSolver   bool     `json:"requires_browser_solver" yaml:"requires_browser_solver"`
	RequiresBrowserDownload bool     `json:"requires_browser_downloader" yaml:"requires_browser_downloader"`
	SingleManga             bool     `json:"single_manga" yaml:"single_manga"`
	Chapterless             bool     `json:"chapterless,omitempty" yaml:"chapterless,omitempty"`
	NSFW                    bool     `json:"nsfw,omitempty" yaml:"nsfw,omitempty"`
	Enabled                 bool     `json:"enabled" yaml:"enabled"`
	Version                 string   `json:"version" yaml:"version"`
}

// VerifyStep is one stage of the verification pipeline with its outcome.
type VerifyStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | failed | skipped
	Detail string `json:"detail,omitempty"`
	Log    string `json:"log,omitempty"`
}

const (
	StepOK      = "ok"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Source is a persisted profile plus its latest verification result.
type Source struct {
	Profile
	Origin            string       `json:"origin"`
	Status            string       `json:"status"`
	LastCheckedAt     string       `json:"last_checked_at,omitempty"`
	LastError         string       `json:"last_error,omitempty"`
	ChaptersFound     int          `json:"chapters_found"`
	SampleImagesFound int          `json:"sample_images_found"`
	ImageExtensions   []string     `json:"image_extensions"`
	VerifySteps       []VerifyStep `json:"verify_steps,omitempty"`
	// ChapterFetch/ImageFetch are the methods learned from the last
	// successful scrape; empty means not yet known.
	ChapterFetch string `json:"chapter_fetch,omitempty"`
	ImageFetch   string `json:"image_fetch,omitempty"`
}

// TemplateProfile returns a starter profile for local editing.
func TemplateProfile(id string) Profile {
	id = strings.ToLower(strings.TrimSpace(id))
	return Profile{
		ID:                id,
		Name:              id,
		BaseURL:           "https://example.com/",
		SampleMangaURL:    "https://example.com/manga/sample/",
		SearchURL:         "https://example.com/search?q={query}",
		Scraper:           "generic",
		AllowedExtensions: []string{"jpg", "jpeg", "png", "webp"},
		MinChapters:       1,
		Enabled:           true,
	}
}

func normalizeProfile(p Profile) (Profile, error) {
	p.ID = strings.ToLower(strings.TrimSpace(p.ID))
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	p.SampleMangaURL = strings.TrimSpace(p.SampleMangaURL)
	p.SearchURL = strings.TrimSpace(p.SearchURL)
	p.Scraper = strings.TrimSpace(p.Scraper)
	p.Version = strings.TrimSpace(p.Version)
	if p.Scraper == "" {
		p.Scraper = "generic"
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	p.Domains = cleanStrings(p.Domains)
	p.AllowedExtensions = cleanExtensions(p.AllowedExtensions)
	p.Languages = cleanStrings(p.Languages)
	if p.Chapterless {
		p.MinChapters = 1
	}
	if p.ID == "" {
		return Profile{}, fmt.Errorf("source id is required")
	}
	switch p.ID {
	case "all", "linked", "unlinked", "imported":
		return Profile{}, fmt.Errorf("source id %q is reserved", p.ID)
	}
	if _, err := url.ParseRequestURI(p.BaseURL); err != nil {
		return Profile{}, fmt.Errorf("invalid base_url for %s: %w", p.ID, err)
	}
	if _, err := url.ParseRequestURI(p.SampleMangaURL); err != nil {
		return Profile{}, fmt.Errorf("invalid sample_manga_url for %s: %w", p.ID, err)
	}
	if p.SearchURL != "" {
		if _, err := url.ParseRequestURI(strings.ReplaceAll(p.SearchURL, "{query}", "sample")); err != nil {
			return Profile{}, fmt.Errorf("invalid search_url for %s: %w", p.ID, err)
		}
	}
	if !registry.Supported(p.Scraper) {
		return Profile{}, fmt.Errorf("unsupported scraper %q for %s", p.Scraper, p.ID)
	}
	if p.MinChapters < 0 {
		p.MinChapters = 0
	}
	return p, nil
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cleanExtensions(values []string) []string {
	out := cleanStrings(values)
	for i := range out {
		out[i] = strings.TrimPrefix(out[i], ".")
	}
	return out
}
