package service

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
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

const (
	matchSourceWorkers    = 8
	matchCandidateWorkers = 4
	sourceMatchTimeout    = 45 * time.Second
	matchCacheTTL         = 24 * time.Hour
)

// MatchSources searches every enabled source for a catalog work, reusing fresh hits and skipping fresh misses.
func (s *CatalogService) MatchSources(ctx context.Context, cfg *config.Config, logSvc ui.Log, catalogID int64) ([]catalog.Match, error) {
	manga, err := s.repo.GetManga(ctx, catalogID)
	if err != nil {
		return nil, err
	}
	if profiles, err := sources.BuiltInProfiles(); err == nil {
		if err := s.sources.Sync(ctx, profiles, sources.OriginBuiltin, true); err != nil {
			return nil, err
		}
	}
	list, err := s.sources.List(ctx)
	if err != nil {
		return nil, err
	}
	reuse, skip := s.recentDecisions(ctx, catalogID)
	var out []catalog.Match
	var todo []sources.Source
	for _, src := range list {
		if !src.Enabled || src.NSFW && !manga.IsAdult {
			continue
		}
		if m, ok := reuse[src.ID]; ok {
			out = append(out, m)
		} else if !skip[src.ID] {
			todo = append(todo, src)
		}
	}
	type result struct {
		match     catalog.Match
		ok, tried bool
	}
	results := make([]result, len(todo))
	parallel(ctx, len(todo), matchSourceWorkers, func(i int) {
		r := &results[i]
		r.match, r.ok, r.tried = s.matchSource(ctx, cfg, logSvc, manga, todo[i])
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, r := range results {
		if r.ok {
			stored, err := s.repo.UpsertMatch(ctx, r.match)
			if err != nil {
				return out, err
			}
			out = append(out, stored)
		} else if r.tried {
			if err := s.repo.RecordMatchMiss(ctx, catalogID, todo[i].ID); err != nil {
				logSvc.Debugf("Recording match miss for %s failed: %v\n", todo[i].ID, err)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b catalog.Match) int {
		return cmp.Or(cmp.Compare(b.ChaptersFound, a.ChaptersFound), cmp.Compare(b.Confidence, a.Confidence))
	})
	return out, nil
}

func parallel(ctx context.Context, n, limit int, fn func(int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				fn(i)
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
}

func (s *CatalogService) recentDecisions(ctx context.Context, catalogID int64) (reuse map[string]catalog.Match, skip map[string]bool) {
	reuse, skip = map[string]catalog.Match{}, map[string]bool{}
	stored, _ := s.repo.ListMatches(ctx, catalogID)
	for _, m := range stored {
		verified, err := database.ParseTime(m.VerifiedAt)
		if m.SourceID == "" || err != nil || time.Since(verified) > matchCacheTTL {
			continue
		}
		if m.ChaptersFound > 0 {
			reuse[m.SourceID] = m
		} else {
			skip[m.SourceID] = true
		}
	}
	return reuse, skip
}

// matchSource reports attempted=false when the source couldn't be searched at all, so no miss gets cached.
func (s *CatalogService) matchSource(ctx context.Context, cfg *config.Config, logSvc ui.Log, manga catalog.Manga, src sources.Source) (match catalog.Match, matched, attempted bool) {
	ctx, cancel := context.WithTimeout(ctx, sourceMatchTimeout)
	defer cancel()
	probeCfg := ConfigForSource(*cfg, src, SourceConfigOptions{})
	downloadSvc, err := NewSourceDownloadService(&probeCfg, logSvc, nil, src.Scraper)
	if err != nil {
		logSvc.Debugf("Source %s scraper setup failed: %v\n", src.ID, err)
		return catalog.Match{}, false, false
	}
	candidates, searched, failed := nativeSearchURLs(ctx, downloadSvc, logSvc, src, manga)
	if !searched && !failed {
		candidates, searched, failed = searchSourceURLs(ctx, probeCfg, logSvc, src, manga)
	}
	if failed {
		return catalog.Match{}, false, false
	}
	method := "search"
	if !searched {
		method = "slug_probe"
		candidates = candidateSourceURLs(src, manga)
		if probeCfg.BrowserSolver.Enabled && len(candidates) > 1 {
			candidates = candidates[:1]
		}
	}
	match, matched = s.bestCandidate(ctx, downloadSvc, logSvc, manga, src, uniqueStrings(candidates), method)
	return match, matched, true
}

func (s *CatalogService) bestCandidate(ctx context.Context, downloadSvc *DownloadService, logSvc ui.Log, manga catalog.Manga, src sources.Source, candidates []string, method string) (catalog.Match, bool) {
	found := make([]catalog.Match, len(candidates))
	ok := make([]bool, len(candidates))
	parallel(ctx, len(candidates), matchCandidateWorkers, func(i int) {
		found[i], ok[i] = verifyCandidate(ctx, downloadSvc, logSvc, manga, src, candidates[i], method)
	})
	var best catalog.Match
	matched := false
	for i := range found {
		if ok[i] && (!matched || found[i].Confidence > best.Confidence) {
			best, matched = found[i], true
		}
	}
	return best, matched
}

func verifyCandidate(ctx context.Context, downloadSvc *DownloadService, logSvc ui.Log, manga catalog.Manga, src sources.Source, sourceURL, method string) (catalog.Match, bool) {
	chapters, _, _, err := downloadSvc.FetchChapters(ctx, sourceURL)
	if err != nil || len(chapters) == 0 {
		logSvc.Debugf("Candidate %s rejected (%d chapters): %v\n", sourceURL, len(chapters), err)
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

// ListMatches returns stored source matches without miss markers.
func (s *CatalogService) ListMatches(ctx context.Context, catalogID int64) ([]catalog.Match, error) {
	all, err := s.repo.ListMatches(ctx, catalogID)
	return slices.DeleteFunc(all, func(m catalog.Match) bool { return m.ChaptersFound <= 0 }), err
}

func (s *CatalogService) TrackMatch(ctx context.Context, matchID int64, outputPath string, monitored bool, refreshInterval string) (library.Title, error) {
	match, err := s.repo.GetMatch(ctx, matchID)
	if err != nil {
		return library.Title{}, err
	}
	manga, err := s.repo.GetManga(ctx, match.CatalogMangaID)
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

// nativeSearchURLs uses the scraper's own search API when it has one; failed means every query errored.
func nativeSearchURLs(ctx context.Context, downloadSvc *DownloadService, logSvc ui.Log, src sources.Source, manga catalog.Manga) (urls []string, searched, failed bool) {
	if strings.TrimSpace(src.SearchURL) == "" && !strings.HasPrefix(src.Scraper, kotatsu.ScraperPrefix) {
		return nil, false, false
	}
	titles := mangaTitleVariants(manga)
	errs := 0
	for _, title := range titles {
		urls, handled, err := downloadSvc.SearchManga(ctx, src.SearchURL, title)
		if !handled {
			return nil, false, false
		}
		if err != nil {
			errs++
			logSvc.Debugf("Native search failed for %s %q: %v\n", src.ID, title, err)
		} else if len(urls) > 0 {
			return urls, true, false
		}
	}
	return nil, true, errs > 0 && errs == len(titles)
}

// searchSourceURLs scrapes the source's search page; failed means it never loaded.
func searchSourceURLs(ctx context.Context, cfg config.Config, logSvc ui.Log, src sources.Source, manga catalog.Manga) (out []string, searched, failed bool) {
	if strings.TrimSpace(src.SearchURL) == "" {
		return nil, false, false
	}
	errs := 0
	for _, title := range mangaTitleVariants(manga) {
		searchURL := strings.ReplaceAll(src.SearchURL, "{query}", url.QueryEscape(title))
		body, finalURL, err := fetchSearchPage(ctx, cfg, searchURL)
		if err != nil {
			errs++
			logSvc.Debugf("Source search failed for %s: %v\n", searchURL, err)
			continue
		}
		searched = true
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
		if err != nil {
			continue
		}
		found := append(searchLinks(doc, src, manga), searchStructuredLinks(body, src, manga)...)
		if len(found) == 0 && finalURL != searchURL && looksLikeMangaResult(src, manga, finalURL, "") {
			found = []string{finalURL}
		}
		logSvc.Debugf("Source search %s %q: %d candidates\n", src.ID, title, len(found))
		if out = append(out, found...); len(out) > 0 {
			break
		}
	}
	return out, searched, !searched && errs > 0
}

// fetchSearchPage is HTTP-only: solver-only sources fall back to slug probing instead of a slow browser search.
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
	finalURL = resp.Request.URL.String()
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
	type hit struct {
		url      string
		chapters int
	}
	var out []hit
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
						out = append(out, hit{url: resolved, chapters: chapters})
					}
				}
			}
			if slug, ok := v["slug"].(string); ok {
				if candidate := sourceURLFromSlug(src, slug); candidate != "" && looksLikeMangaResult(src, manga, candidate, text) {
					out = append(out, hit{url: candidate, chapters: chapters})
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
	slices.SortStableFunc(out, func(a, b hit) int { return cmp.Compare(b.chapters, a.chapters) })
	values := make([]string, 0, len(out))
	for _, candidate := range out {
		values = append(values, candidate.url)
	}
	return uniqueStrings(values)
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
	for _, t := range []string{m.TitleEnglish, m.TitleRomaji, m.TitleNative} {
		if t = strings.TrimSpace(t); t != "" {
			return t
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

// hasNumericOrOpaqueNeighbor rejects sample URLs whose identifying segment is an id rather than a slug.
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
		if d := *m.Chapters - chapters; d >= -2 && d <= 2 {
			score += 0.15
		}
	} else if chapters > 0 {
		score += 0.05
	}
	return min(score, 1)
}

func sameHostURL(a, b *url.URL) bool {
	return strings.TrimPrefix(strings.ToLower(a.Host), "www.") == strings.TrimPrefix(strings.ToLower(b.Host), "www.")
}
