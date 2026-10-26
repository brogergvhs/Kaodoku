package service

import (
	"context"
	"net/url"
	"os"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

// SourceConfigOptions controls how a source profile is applied to runtime config.
type SourceConfigOptions struct {
	PreserveAllowedExtensions bool
}

// ConfigForSource applies scraper runtime requirements from a source profile.
func ConfigForSource(cfg config.Config, src sources.Source, opts SourceConfigOptions) config.Config {
	if len(src.AllowedExtensions) > 0 && !opts.PreserveAllowedExtensions {
		cfg.AllowExt = src.AllowedExtensions
	}
	if len(src.Languages) > 0 {
		cfg.Languages = src.Languages
	}
	cfg.Chapterless = src.Chapterless
	// A learned method wins over the runtime default; a hard profile
	// requirement is always honored, even over a learned "http".
	switch src.ChapterFetch {
	case sources.FetchSolver:
		cfg.BrowserSolver.Enabled = true
	case sources.FetchHTTP:
		cfg.BrowserSolver.Enabled = src.RequiresBrowserSolver
	default:
		cfg.BrowserSolver.Enabled = cfg.BrowserSolver.Enabled || src.RequiresBrowserSolver
	}
	switch src.ImageFetch {
	case sources.FetchBrowser:
		cfg.BrowserDownload.Enabled = true
	case sources.FetchHTTP:
		cfg.BrowserDownload.Enabled = src.RequiresBrowserDownload
	default:
		cfg.BrowserDownload.Enabled = cfg.BrowserDownload.Enabled || src.RequiresBrowserDownload
	}
	return cfg
}

// ResolveSourceForURL finds the best enabled source profile for a URL.
func ResolveSourceForURL(ctx context.Context, target, dbPath string, logSvc ui.Log) (sources.Source, bool) {
	profiles, err := sources.BuiltInProfiles()
	if err != nil {
		if logSvc != nil {
			logSvc.Debugf("Load builtin sources failed: %v\n", err)
		}
		return sources.Source{}, false
	}

	candidates := make([]sources.Source, 0, len(profiles))
	for _, profile := range profiles {
		candidates = append(candidates, sourceFromProfile(profile, sources.OriginBuiltin))
	}

	dbSources, err := loadDBSources(ctx, dbPath, profiles)
	if err != nil {
		if logSvc != nil {
			logSvc.Debugf("Load DB sources failed: %v\n", err)
		}
	} else if len(dbSources) > 0 {
		candidates = mergeSources(candidates, dbSources)
	}

	return MatchSourceForURL(candidates, target)
}

// MatchSourceForURL returns the best source match from a caller-provided list.
func MatchSourceForURL(list []sources.Source, target string) (sources.Source, bool) {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return sources.Source{}, false
	}

	bestScore := 0
	var best sources.Source
	for _, src := range list {
		// sourceMatchScore already folds originRank into the score.
		if score := sourceMatchScore(src, u); score > bestScore {
			bestScore = score
			best = src
		}
	}
	return best, bestScore > 0
}

func loadDBSources(ctx context.Context, dbPath string, builtins []sources.Profile) ([]sources.Source, error) {
	if dbPath == "" {
		dbPath = database.DefaultPath()
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	db, err := database.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		return nil, err
	}
	repo := sources.NewRepository(db)
	if err := repo.Sync(ctx, builtins, sources.OriginBuiltin, true); err != nil {
		return nil, err
	}
	return repo.List(ctx)
}

func sourceFromProfile(profile sources.Profile, origin string) sources.Source {
	return sources.Source{
		Profile: profile,
		Origin:  origin,
		Status:  sources.StatusHealthy,
	}
}

func mergeSources(base, overrides []sources.Source) []sources.Source {
	byID := map[string]int{}
	out := append([]sources.Source{}, base...)
	for i, src := range out {
		byID[src.ID] = i
	}
	for _, src := range overrides {
		if i, ok := byID[src.ID]; ok {
			out[i] = src
			continue
		}
		byID[src.ID] = len(out)
		out = append(out, src)
	}
	return out
}

func sourceMatchScore(src sources.Source, target *url.URL) int {
	if !src.Enabled {
		return 0
	}

	score := 0
	targetHost := normalizeHost(target.Host)
	for _, domain := range src.Domains {
		if hostMatches(domain, targetHost) {
			score = max(score, 100)
		}
	}
	score = max(score, urlMatchScore(src.BaseURL, target, 120))
	score = max(score, urlMatchScore(src.SampleMangaURL, target, 140))
	if score == 0 {
		return 0
	}
	return score + originRank(src.Origin)
}

func urlMatchScore(raw string, target *url.URL, baseScore int) int {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !hostMatches(u.Host, target.Host) {
		return 0
	}
	return baseScore + commonPathPrefixScore(u.Path, target.Path)
}

func commonPathPrefixScore(a, b string) int {
	aParts := sourcePathParts(a)
	bParts := sourcePathParts(b)
	score := 0
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if aParts[i] != bParts[i] {
			break
		}
		score += 5
	}
	return score
}

func sourcePathParts(path string) []string {
	return strings.FieldsFunc(strings.Trim(path, "/"), func(r rune) bool { return r == '/' })
}

func hostMatches(candidate, target string) bool {
	candidate = normalizeHost(candidate)
	target = normalizeHost(target)
	return candidate == target || strings.TrimPrefix(candidate, "www.") == strings.TrimPrefix(target, "www.")
}

func normalizeHost(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if host, _, ok := strings.Cut(value, ":"); ok {
		return host
	}
	return value
}

func originRank(origin string) int {
	switch origin {
	case sources.OriginLocal:
		return 3
	case sources.OriginRegistry:
		return 2
	case sources.OriginBuiltin:
		return 1
	default:
		return 0
	}
}
