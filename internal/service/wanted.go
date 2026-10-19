package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/ui"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// WantedService coordinates canonical manga and source matching.
type WantedService struct {
	catalog *catalog.Repository
	sources *sources.Repository
	library *library.Repository
	anilist interface {
		Search(context.Context, string, int, catalog.SearchFilter) ([]catalog.Manga, bool, error)
		Get(context.Context, int) (catalog.Manga, error)
		TagVocabulary(context.Context) (genres []string, tags []catalog.ContentTag, err error)
		Related(context.Context, int, int) ([]catalog.Manga, error)
		Trending(context.Context, int) ([]catalog.Manga, error)
		GetWithRelations(context.Context, int) (catalog.Manga, []catalog.Relation, error)
		UserList(context.Context, int) ([]catalog.AniListEntry, error)
		MediaEntry(context.Context, int) (int, string, bool, error)
		SaveEntry(context.Context, int, int, string) error
		DeleteEntry(context.Context, int, int) error
		FavouriteManga(context.Context, int) ([]int, error)
		IsFavourite(context.Context, int) (bool, error)
		ToggleFavourite(context.Context, int) error
	}
}

// AniListAPI is the catalog metadata client surface used by services.
type AniListAPI interface {
	Get(context.Context, int) (catalog.Manga, error)
	TagVocabulary(context.Context) (genres []string, tags []catalog.ContentTag, err error)
	Related(context.Context, int, int) ([]catalog.Manga, error)
	Trending(context.Context, int) ([]catalog.Manga, error)
	GetWithRelations(context.Context, int) (catalog.Manga, []catalog.Relation, error)
	UserList(context.Context, int) ([]catalog.AniListEntry, error)
	MediaEntry(context.Context, int) (int, string, bool, error)
	SaveEntry(context.Context, int, int, string) error
	DeleteEntry(context.Context, int, int) error
	FavouriteManga(context.Context, int) ([]int, error)
	IsFavourite(context.Context, int) (bool, error)
	ToggleFavourite(context.Context, int) error
}

// AniList exposes the catalog metadata client.
func (s *WantedService) AniList() AniListAPI {
	return s.anilist
}

// OpenWanted opens the app database for wanted-title workflows.
func OpenWanted(ctx context.Context, dbPath string) (*WantedService, func(), error) {
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := database.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return newWantedService(db), func() { _ = db.Close() }, nil
}

func newWantedService(db database.Handle) *WantedService {
	return &WantedService{
		catalog: catalog.NewRepository(db),
		sources: sources.NewRepository(db),
		library: library.NewRepository(db),
		anilist: catalog.NewAniListClient(nil),
	}
}

// SearchAniList searches AniList and stores returned metadata locally.
func (s *WantedService) SearchAniList(ctx context.Context, query string, limit int, filter catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	items, more, err := s.anilist.Search(ctx, strings.TrimSpace(query), limit, filter)
	if err != nil {
		return nil, false, err
	}
	out := make([]catalog.Manga, 0, len(items))
	for _, item := range items {
		stored, err := s.catalog.UpsertManga(ctx, item)
		if err != nil {
			return nil, false, err
		}
		out = append(out, stored)
	}
	return out, more, nil
}

// ErrContentBlocked marks content the acting user's guard forbids.
var ErrContentBlocked = errors.New("content blocked")

// AddAniListWanted fetches an AniList title, stores it, and marks it wanted.
// A non-nil allowed guard runs before anything is persisted.
func (s *WantedService) AddAniListWanted(ctx context.Context, anilistID int, allowed func(catalog.Manga) bool) (catalog.Manga, error) {
	item, err := s.anilist.Get(ctx, anilistID)
	if err != nil {
		return catalog.Manga{}, err
	}
	if allowed != nil && !allowed(item) {
		return catalog.Manga{}, ErrContentBlocked
	}
	item.Wanted = true
	return s.catalog.UpsertManga(ctx, item)
}

// ListWanted returns wanted canonical titles.
func (s *WantedService) ListWanted(ctx context.Context) ([]catalog.Manga, error) {
	return s.catalog.ListWanted(ctx)
}

// MatchSources finds source candidates for one wanted title.
func (s *WantedService) MatchSources(ctx context.Context, cfg *config.Config, logSvc ui.Log, catalogID int64) ([]catalog.Match, error) {
	manga, err := s.catalog.GetManga(ctx, catalogID)
	if err != nil {
		return nil, err
	}
	if profiles, err := sources.BuiltInProfiles(); err == nil {
		if err := s.sources.Sync(ctx, profiles, sources.OriginBuiltin, true); err != nil {
			return nil, err
		}
	}
	sourceList, err := s.sources.List(ctx)
	if err != nil {
		return nil, err
	}
	reuse, skip := s.recentDecisions(ctx, catalogID)
	var enabled []sources.Source
	var cached []catalog.Match
	for _, src := range sourceList {
		if !src.Enabled {
			continue
		}
		if src.NSFW && !manga.IsAdult {
			continue // NSFW-only sources never host SFW manga
		}
		if match, ok := reuse[src.ID]; ok {
			logSvc.Debugf("Reusing cached match for source %s (verified %s).\n", src.ID, match.VerifiedAt)
			cached = append(cached, match)
			continue
		}
		if skip[src.ID] {
			logSvc.Debugf("Skipping source %s: recently found no match.\n", src.ID)
			continue
		}
		enabled = append(enabled, src)
	}
	matches := make(chan catalog.Match, len(enabled))
	jobs := make(chan sources.Source)
	var wg sync.WaitGroup
	var mu sync.Mutex
	attempted := map[string]bool{}
	for range min(len(enabled), matchSourceWorkers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for src := range jobs {
				match, ok, tried := s.matchSource(ctx, cfg, logSvc, manga, src)
				mu.Lock()
				attempted[src.ID] = tried
				mu.Unlock()
				if ok {
					select {
					case matches <- match:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	for _, src := range enabled {
		select {
		case jobs <- src:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(matches)
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	close(matches)

	out := cached
	matched := map[string]bool{}
	for match := range matches {
		stored, err := s.catalog.UpsertMatch(ctx, match)
		if err != nil {
			return out, err
		}
		matched[stored.SourceID] = true
		out = append(out, stored)
	}
	for _, src := range enabled {
		if matched[src.ID] || !attempted[src.ID] {
			continue
		}
		if err := s.catalog.RecordMatchMiss(ctx, catalogID, src.ID); err != nil {
			logSvc.Debugf("Recording match miss for %s failed: %v\n", src.ID, err)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ChaptersFound != out[j].ChaptersFound {
			return out[i].ChaptersFound > out[j].ChaptersFound
		}
		return out[i].Confidence > out[j].Confidence
	})
	return out, nil
}

// matchCacheTTL is how long a match attempt is reused before re-scraping.
const matchCacheTTL = 24 * time.Hour

// recentDecisions partitions recent attempts per source into successes to
// reuse and misses to skip, from a single query.
func (s *WantedService) recentDecisions(ctx context.Context, catalogID int64) (reuse map[string]catalog.Match, skip map[string]bool) {
	reuse, skip = map[string]catalog.Match{}, map[string]bool{}
	stored, err := s.catalog.ListMatches(ctx, catalogID)
	if err != nil {
		return
	}
	for _, match := range stored {
		if match.SourceID == "" {
			continue
		}
		verifiedAt, err := database.ParseTime(match.VerifiedAt)
		if err != nil || time.Since(verifiedAt) > matchCacheTTL {
			continue
		}
		if match.ChaptersFound > 0 {
			reuse[match.SourceID] = match
		} else {
			skip[match.SourceID] = true
		}
	}
	return
}

const (
	matchSourceWorkers    = 8
	matchCandidateWorkers = 4
	sourceMatchTimeout    = 45 * time.Second
)

// attempted is false when the source could not be searched at all, such runs must not cache a miss.
func (s *WantedService) matchSource(ctx context.Context, cfg *config.Config, logSvc ui.Log, manga catalog.Manga, src sources.Source) (match catalog.Match, matched, attempted bool) {
	ctx, cancel := context.WithTimeout(ctx, sourceMatchTimeout)
	defer cancel()

	probeCfg := ConfigForSource(*cfg, src, SourceConfigOptions{})
	downloadSvc, err := NewSourceDownloadService(&probeCfg, logSvc, nil, src.Scraper)
	if err != nil {
		if logSvc != nil {
			logSvc.Debugf("Source %s scraper setup failed: %v\n", src.ID, err)
		}
		return catalog.Match{}, false, false
	}

	candidates, searched, failed := nativeSearchURLs(ctx, downloadSvc, logSvc, src, manga)
	if failed {
		return catalog.Match{}, false, false
	}
	if !searched {
		candidates, searched, failed = searchSourceURLs(ctx, probeCfg, logSvc, src, manga)
		if failed {
			return catalog.Match{}, false, false
		}
	}
	method := "search"
	if !searched {
		method = "slug_probe"
		guesses := candidateSourceURLs(src, manga)
		// On a solver source each probe costs a full CF solve, so only the
		// best guess is worth trying.
		if probeCfg.BrowserSolver.Enabled && len(guesses) > 1 {
			guesses = guesses[:1]
		}
		candidates = append(candidates, guesses...)
	} else if len(candidates) == 0 && logSvc != nil {
		logSvc.Debugf("Source search %s completed with no candidates; skipping guessed slug probes.\n", src.ID)
	}
	match, matched = s.bestCandidate(ctx, downloadSvc, logSvc, manga, src, uniqueStrings(candidates), method)
	return match, matched, true
}

// bestCandidate verifies candidates concurrently (bounded) and returns the
// highest-confidence match; the per-source timeout bounds the total work.
func (s *WantedService) bestCandidate(ctx context.Context, downloadSvc *DownloadService, logSvc ui.Log, manga catalog.Manga, src sources.Source, candidates []string, method string) (catalog.Match, bool) {
	found := make([]catalog.Match, len(candidates))
	ok := make([]bool, len(candidates))
	sem := make(chan struct{}, matchCandidateWorkers)
	var wg sync.WaitGroup
	for i, candidate := range candidates {
		wg.Add(1)
		go func(i int, candidate string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			found[i], ok[i] = s.verifyCandidate(ctx, downloadSvc, logSvc, manga, src, candidate, method)
		}(i, candidate)
	}
	wg.Wait()

	best := catalog.Match{}
	matched := false
	for i := range candidates {
		if ok[i] && (!matched || found[i].Confidence > best.Confidence) {
			best, matched = found[i], true
		}
	}
	return best, matched
}

// ListMatches returns persisted source matches, excluding miss markers.
func (s *WantedService) ListMatches(ctx context.Context, catalogID int64) ([]catalog.Match, error) {
	all, err := s.catalog.ListMatches(ctx, catalogID)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, m := range all {
		if m.ChaptersFound > 0 {
			out = append(out, m)
		}
	}
	return out, nil
}

// TrackMatch adds a selected source match to the library.
func (s *WantedService) TrackMatch(ctx context.Context, matchID int64, outputPath string, monitored bool, refreshInterval string) (library.Title, error) {
	match, err := s.catalog.GetMatch(ctx, matchID)
	if err != nil {
		return library.Title{}, err
	}
	manga, err := s.catalog.GetManga(ctx, match.CatalogMangaID)
	if err != nil {
		return library.Title{}, err
	}
	return s.library.AddTitle(ctx, library.AddTitleParams{
		CatalogMangaID:  &manga.ID,
		SourceID:        match.SourceID,
		SourceURL:       match.SourceURL,
		DisplayTitle:    displayMangaTitle(manga),
		OutputPath:      outputPath,
		Monitored:       monitored,
		RefreshInterval: refreshInterval,
	})
}

func (s *WantedService) verifyCandidate(ctx context.Context, downloadSvc *DownloadService, logSvc ui.Log, manga catalog.Manga, src sources.Source, sourceURL, method string) (catalog.Match, bool) {
	chapters, _, _, err := downloadSvc.FetchChapters(ctx, sourceURL)
	if err != nil {
		if logSvc != nil {
			logSvc.Debugf("Candidate %s failed chapter fetch: %v\n", sourceURL, err)
		}
		return catalog.Match{}, false
	}
	if len(chapters) == 0 {
		if logSvc != nil {
			logSvc.Debugf("Candidate %s has no chapters.\n", sourceURL)
		}
		return catalog.Match{}, false
	}
	return catalog.Match{
		CatalogMangaID: manga.ID,
		SourceID:       src.ID,
		SourceURL:      sourceURL,
		Title:          displayMangaTitle(manga),
		Confidence:     matchConfidence(manga, sourceURL, len(chapters)),
		MatchMethod:    method,
		ChaptersFound:  len(chapters),
	}, true
}

// nativeSearchURLs uses the scraper's own search API when it has one
// generic HTML search scrape can't read JSON APIs and its slug-probe fallback
// fabricates wrong URLs on sites where the path carries an id.
// failed is true when every query errored, so "no results" can't be trusted.
func nativeSearchURLs(ctx context.Context, downloadSvc *DownloadService, logSvc ui.Log, src sources.Source, manga catalog.Manga) (urls []string, searched, failed bool) {
	if strings.TrimSpace(src.SearchURL) == "" && !strings.HasPrefix(src.Scraper, kotatsu.ScraperPrefix) {
		return nil, false, false
	}
	errs, tried := 0, 0
	for _, title := range mangaTitleVariants(manga) {
		tried++
		urls, handled, err := downloadSvc.SearchManga(ctx, src.SearchURL, title)
		if !handled {
			return nil, false, false
		}
		if err != nil {
			errs++
			if logSvc != nil {
				logSvc.Debugf("Native search failed for %s %q: %v\n", src.ID, title, err)
			}
			continue
		}
		if len(urls) > 0 {
			return urls, true, false
		}
	}
	return nil, true, errs > 0 && errs == tried
}

// failed is true when the search page never loaded, so the source can't be
// told apart from one that simply has no match.
func searchSourceURLs(ctx context.Context, cfg config.Config, logSvc ui.Log, src sources.Source, manga catalog.Manga) (out []string, searched, failed bool) {
	if strings.TrimSpace(src.SearchURL) == "" {
		if logSvc != nil {
			logSvc.Debugf("Source search skipped for %s: no search_url.\n", src.ID)
		}
		return nil, false, false
	}
	errs := 0
	for _, title := range mangaTitleVariants(manga) {
		searchURL := strings.ReplaceAll(src.SearchURL, "{query}", url.QueryEscape(title))
		if logSvc != nil {
			logSvc.Debugf("Source search %s query %q: %s\n", src.ID, title, searchURL)
		}
		body, finalURL, err := fetchSearchPage(ctx, cfg, searchURL)
		if err != nil {
			errs++
			if logSvc != nil {
				logSvc.Debugf("Source search failed for %s: %v\n", searchURL, err)
			}
			continue
		}
		searched = true
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
		if err != nil {
			if logSvc != nil {
				logSvc.Debugf("Source search parse failed for %s (%d bytes): %v\n", searchURL, len(body), err)
			}
			continue
		}
		found := append(searchLinks(doc, src, manga), searchStructuredLinks(body, src, manga)...)
		if len(found) == 0 && finalURL != searchURL && looksLikeMangaResult(src, manga, finalURL, "") {
			// A single-result search redirected straight to the manga page.
			found = []string{finalURL}
		}
		if logSvc != nil {
			logSvc.Debugf("Source search %s returned %d candidates from %d bytes.\n", src.ID, len(uniqueStrings(found)), len(body))
		}
		out = append(out, found...)
		if len(out) > 0 {
			break
		}
	}
	if len(out) == 0 && logSvc != nil {
		logSvc.Debugf("Source search %s found no candidates for %q.\n", src.ID, displayMangaTitle(manga))
	}
	return out, searched, !searched && errs > 0
}

// fetchSearchPage fetches a source search page over HTTP only;
// solver-only source falls through to slug probing rather than paying the
// slow browser path for search.
func fetchSearchPage(ctx context.Context, cfg config.Config, target string) (body, finalURL string, err error) {
	client, err := util.NewHTTPClient(util.HTTPClientOptions{
		Timeout:              30 * time.Second,
		UserAgent:            util.PickUserAgent(cfg.UserAgent),
		Cookie:               cfg.Cookie,
		CookieFile:           cfg.CookieFile,
		Transport:            cloudflareTransport,
		RateLimit:            hostRateLimit(&cfg),
		BlockPrivateNetworks: true,
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	finalURL = target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", finalURL, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	return string(data), finalURL, err
}

func searchLinks(doc *goquery.Document, src sources.Source, manga catalog.Manga) []string {
	base, err := url.Parse(src.BaseURL)
	if err != nil {
		return nil
	}
	var out []string
	doc.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		resolved := resolveMatchURL(src.BaseURL, href)
		u, err := url.Parse(resolved)
		if err != nil || u.Host == "" || !sameHostURL(base, u) {
			return
		}
		text := strings.Join(strings.Fields(a.Text()), " ")
		if !looksLikeMangaResult(src, manga, resolved, text) {
			return
		}
		out = append(out, resolved)
	})
	return uniqueStrings(out)
}

func searchStructuredLinks(body string, src sources.Source, manga catalog.Manga) []string {
	var raw any
	if json.Unmarshal([]byte(body), &raw) != nil {
		return nil
	}
	var out []sourceCandidate
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			text := jsonResultText(v)
			chapters := jsonNumber(v["chapter_count"])
			for _, key := range []string{"public_url", "permalink", "href"} {
				if candidate, ok := v[key].(string); ok {
					resolved := resolveMatchURL(src.BaseURL, candidate)
					if looksLikeMangaResult(src, manga, resolved, text) {
						out = append(out, sourceCandidate{url: resolved, chapters: chapters})
					}
				}
			}
			if slug, ok := v["slug"].(string); ok {
				if candidate := sourceURLFromSlug(src, slug); candidate != "" && looksLikeMangaResult(src, manga, candidate, text) {
					out = append(out, sourceCandidate{url: candidate, chapters: chapters})
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(raw)
	sort.SliceStable(out, func(i, j int) bool { return out[i].chapters > out[j].chapters })
	values := make([]string, 0, len(out))
	for _, candidate := range out {
		values = append(values, candidate.url)
	}
	return uniqueStrings(values)
}

type sourceCandidate struct {
	url      string
	chapters int
}

func jsonNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	default:
		return 0
	}
}

func jsonResultText(value any) string {
	var parts []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			parts = append(parts, x)
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			for key, item := range x {
				switch key {
				case "title", "titles", "name", "slug", "limited_titles":
					walk(item)
				}
			}
		}
	}
	walk(value)
	return strings.Join(parts, " ")
}

func sourceURLFromSlug(src sources.Source, slug string) string {
	base, err := url.Parse(src.BaseURL)
	if err != nil || base.Host == "" {
		return ""
	}
	sample, err := url.Parse(src.SampleMangaURL)
	if err != nil {
		return ""
	}
	parts := util.PathSegments(sample.Path)
	if len(parts) == 0 {
		return ""
	}
	base.Path = "/" + path.Join(parts[0], strings.Trim(slug, "/"))
	base.RawQuery = ""
	base.Fragment = ""
	return base.String()
}

func looksLikeMangaResult(src sources.Source, manga catalog.Manga, href, text string) bool {
	if !matchesWantedTitle(manga, href+" "+text) {
		return false
	}
	u, err := url.Parse(href)
	if err != nil {
		return false
	}
	sample, err := url.Parse(src.SampleMangaURL)
	if err != nil {
		return true
	}
	candidateParts, sampleParts := util.PathSegments(u.Path), util.PathSegments(sample.Path)
	if len(candidateParts) == 0 || len(sampleParts) == 0 {
		return false
	}
	return candidateParts[0] == sampleParts[0] && len(candidateParts) == len(sampleParts)
}

func candidateSourceURLs(src sources.Source, manga catalog.Manga) []string {
	u, err := url.Parse(src.SampleMangaURL)
	if err != nil || u.Host == "" {
		return nil
	}
	parts := util.PathSegments(u.Path)
	if len(parts) == 0 || hasNumericOrOpaqueNeighbor(parts) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, title := range mangaTitleVariants(manga) {
		slug := slugify(title)
		if slug == "" {
			continue
		}
		next := *u
		nextParts := append([]string{}, parts...)
		nextParts[len(nextParts)-1] = slug
		next.Path = "/" + path.Join(nextParts...)
		next.RawQuery = ""
		value := next.String()
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func resolveMatchURL(baseURL, href string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return href
	}
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return href
	}
	u.Fragment = ""
	return base.ResolveReference(u).String()
}

func mangaTitleVariants(m catalog.Manga) []string {
	return cleanMatchStrings(append([]string{m.TitleEnglish, m.TitleRomaji, m.TitleNative}, m.Synonyms...))
}

func cleanMatchStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func matchesWantedTitle(m catalog.Manga, value string) bool {
	value = strings.ToLower(value)
	for _, title := range mangaTitleVariants(m) {
		var words []string
		for _, word := range strings.Fields(strings.ReplaceAll(slugify(title), "-", " ")) {
			if len(word) >= 3 {
				words = append(words, word)
			}
		}
		if len(words) == 0 {
			continue
		}
		hits := 0
		for _, word := range words {
			if strings.Contains(value, word) {
				hits++
			}
		}
		if hits >= len(words)/2+1 {
			return true
		}
	}
	return false
}

func displayMangaTitle(m catalog.Manga) string {
	for _, value := range []string{m.TitleEnglish, m.TitleRomaji, m.TitleNative} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return m.Provider + ":" + m.ProviderID
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

var (
	reOpaqueSegment = regexp.MustCompile(`^[0-9A-Za-z]{12,}$`)
	reUUIDSegment   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// hasNumericOrOpaqueNeighbor rejects sample URLs whose identifying segment is
// an id rather than a slug
func hasNumericOrOpaqueNeighbor(parts []string) bool {
	if len(parts) < 2 {
		return false
	}
	prev := parts[len(parts)-2]
	if _, err := strconv.Atoi(prev); err == nil {
		return true
	}
	return reOpaqueSegment.MatchString(prev) || reUUIDSegment.MatchString(prev)
}

func matchConfidence(m catalog.Manga, sourceURL string, chapters int) float64 {
	score := 0.5
	urlSlug := strings.ToLower(sourceURL)
	for _, title := range mangaTitleVariants(m) {
		if slug := slugify(title); slug != "" && strings.Contains(urlSlug, slug) {
			score += 0.35
			break
		}
	}
	if m.Chapters != nil && *m.Chapters > 0 {
		diff := *m.Chapters - chapters
		if diff < 0 {
			diff = -diff
		}
		if diff <= 2 {
			score += 0.15
		}
	} else if chapters > 0 {
		score += 0.05
	}
	if score > 1 {
		return 1
	}
	return score
}

func sameHostURL(a, b *url.URL) bool {
	return strings.TrimPrefix(strings.ToLower(a.Host), "www.") == strings.TrimPrefix(strings.ToLower(b.Host), "www.")
}
