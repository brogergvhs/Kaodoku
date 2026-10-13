// Package service contains reusable application use cases shared by the CLI and
// the future daemon/web UI.
package service

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brogergvhs/kaodoku/internal/browserdownload"
	"github.com/brogergvhs/kaodoku/internal/browserfetch"
	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/downloader"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/providers/generic"
	"github.com/brogergvhs/kaodoku/internal/providers/registry"
	"github.com/brogergvhs/kaodoku/internal/ui"
	"github.com/brogergvhs/kaodoku/internal/util"

	cloudflarebp "github.com/DaRealFreak/cloudflare-bp-go"
)

// ProgressHandle receives per-chapter download progress.
type ProgressHandle interface {
	SetTotal(total int)
	Update(done, total int, bytes int64)
	MarkDone()
}

// ProgressManager creates progress handles for concurrently downloaded chapters.
type ProgressManager interface {
	Register(prefix string) ProgressHandle
	Close()
}

// ChapterSelection describes how chapter lists should be filtered.
type ChapterSelection struct {
	Chapter      string
	Range        string
	ExcludeRange string
	List         string
	ExcludeList  string
}

// DownloadSummary describes the outcome of a batch download.
type DownloadSummary struct {
	Chapters       int64
	Images         int64
	Bytes          int64
	FailedChapters int64
	Errors         []string
	Duration       time.Duration
}

// ChapterDownloadResult describes one completed chapter download.
type ChapterDownloadResult struct {
	Chapter    chapters.Chapter
	OutputFile string
	Images     int
	Bytes      int64
}

// DownloadService coordinates scraping, chapter selection, and CBZ downloads.
type DownloadService struct {
	cfg       *config.Config
	client    *http.Client
	scraper   providers.Scraper
	log       ui.Log
	progress  ProgressManager
	browser   downloader.BrowserFetcher
	comicInfo *util.ComicInfo
}

// NewDownloadService creates a service from explicit dependencies.
func NewDownloadService(
	cfg *config.Config,
	client *http.Client,
	scraper providers.Scraper,
	log ui.Log,
	progress ProgressManager,
) *DownloadService {
	if log == nil {
		log = ui.Discard()
	}

	return &DownloadService{
		cfg:      cfg,
		client:   client,
		scraper:  scraper,
		log:      log,
		progress: progress,
	}
}

// SetProgressManager sets or replaces the progress manager used by Download.
func (s *DownloadService) SetProgressManager(progress ProgressManager) {
	s.progress = progress
}

// SetComicInfo sets the series-level ComicInfo template embedded into every
// downloaded CBZ; per-chapter fields are filled at write time.
func (s *DownloadService) SetComicInfo(ci *util.ComicInfo) {
	s.comicInfo = ci
}

func (s *DownloadService) comicInfoEntry(ch chapters.Chapter, pages int) map[string][]byte {
	ci := util.ComicInfo{Manga: "Yes"}
	if s.comicInfo != nil {
		ci = *s.comicInfo
	}
	ci.Title = ch.Title
	ci.Number = ch.Label
	ci.PageCount = pages
	body, err := util.MarshalComicInfo(ci)
	if err != nil {
		return nil
	}
	return map[string][]byte{util.ComicInfoName: body}
}

// NewDefaultDownloadService creates the default HTTP client and generic scraper.
func NewDefaultDownloadService(
	cfg *config.Config,
	log ui.Log,
	progress ProgressManager,
) (*DownloadService, error) {
	return newDownloadServiceWithScraper(cfg, log, progress, "generic")
}

// NewSourceDownloadService creates a download service using a source profile's scraper.
func NewSourceDownloadService(
	cfg *config.Config,
	log ui.Log,
	progress ProgressManager,
	scraperName string,
) (*DownloadService, error) {
	return newDownloadServiceWithScraper(cfg, log, progress, scraperName)
}

func newDownloadServiceWithScraper(
	cfg *config.Config,
	log ui.Log,
	progress ProgressManager,
	scraperName string,
) (*DownloadService, error) {
	browserState := &util.BrowserState{}
	client, err := util.NewHTTPClient(util.HTTPClientOptions{
		Timeout:              30 * time.Second,
		UserAgent:            util.PickUserAgent(cfg.UserAgent),
		Cookie:               cfg.Cookie,
		Transport:            cloudflarebp.AddCloudFlareByPass(http.DefaultTransport),
		CookieFile:           cfg.CookieFile,
		State:                browserState,
		RateLimit:            hostRateLimit(cfg),
		BlockPrivateNetworks: !strings.HasPrefix(scraperName, kotatsu.ScraperPrefix),
		DebugLogger:          log,
	})
	if err != nil {
		return nil, fmt.Errorf("create HTTP client: %w", err)
	}

	// HTML fetches escalate http -> FlareSolverr only; the Selenium
	// downloader is reserved for image capture, never HTML.
	var browser generic.BrowserFetcher
	var downloadBrowser downloader.BrowserFetcher
	if cfg.BrowserSolver.Enabled {
		if cfg.BrowserSolver.Provider != browserfetch.ProviderFlareSolverr {
			return nil, fmt.Errorf("unsupported browser solver provider %q", cfg.BrowserSolver.Provider)
		}
		timeout := time.Duration(cfg.BrowserSolver.TimeoutSeconds) * time.Second
		solver := flaresolverrFetcher{
			client:   browserfetch.NewFlareSolverr(cfg.BrowserSolver.Endpoint, timeout, nil),
			http:     client,
			state:    browserState,
			cache:    browserCookieCache{dbPath: cookieDBPath(cfg)},
			endpoint: cfg.BrowserSolver.Endpoint,
			timeout:  timeout,
			log:      log,
		}
		browser = solver
		downloadBrowser = solver
	}
	var worker *kotatsu.Client
	if cfg.KotatsuWorker.Enabled {
		worker = kotatsu.New(cfg.KotatsuWorker.Endpoint, cfg.KotatsuWorker.APIKey)
	}
	scraper, err := registry.New(scraperName, client, log, cfg.AllowExt, cfg.CheckJS, browser, worker)
	if err != nil {
		return nil, err
	}

	svc := NewDownloadService(cfg, client, scraper, log, progress)
	svc.browser = downloadBrowser
	return svc, nil
}

func hostRateLimit(cfg *config.Config) util.HostRateLimit {
	return util.HostRateLimit{
		Interval: time.Duration(cfg.RateLimit.IntervalMS) * time.Millisecond,
		Burst:    cfg.RateLimit.Burst,
		Disabled: cfg.RateLimit.Disabled,
	}
}

func cookieDBPath(cfg *config.Config) string {
	if cfg.CookieDBPath != "" {
		return cfg.CookieDBPath
	}
	return database.DefaultPath()
}

type flaresolverrFetcher struct {
	client   *browserfetch.FlareSolverr
	http     *http.Client
	state    *util.BrowserState
	cache    browserCookieCache
	endpoint string
	timeout  time.Duration
	log      ui.Log
}

func (f flaresolverrFetcher) LoadCached(ctx context.Context, target string) {
	session, err := f.cache.load(ctx, target)
	if err != nil {
		if f.log != nil {
			f.log.Debugf("Browser cookie cache load failed for %s: %v\n", target, err)
		}
		return
	}
	if session.userAgent != "" {
		f.state.SetUserAgent(session.userAgent)
	}
	if f.http != nil && f.http.Jar != nil && len(session.cookies) > 0 {
		u, err := url.Parse(target)
		if err == nil {
			f.http.Jar.SetCookies(u, session.cookies)
		}
	}
	if f.log != nil && len(session.cookies) > 0 {
		f.log.Infof("Loaded %d cached browser cookies for %s.\n", len(session.cookies), target)
	}
}

func (f flaresolverrFetcher) Fetch(ctx context.Context, target string) (string, error) {
	start := time.Now()
	if f.log != nil {
		f.log.Infof("FlareSolverr request.get %s via %s (timeout=%s).\n", target, f.endpoint, f.timeout)
	}
	result, err := f.client.Fetch(ctx, target)
	if err != nil {
		if f.log != nil {
			f.log.Errorf("FlareSolverr failed for %s after %s: %v\n", target, time.Since(start).Round(time.Millisecond), err)
		}
		return "", err
	}
	if f.log != nil {
		f.log.Infof("FlareSolverr solved %s with status %d in %s (%d bytes).\n", target, result.Status, time.Since(start).Round(time.Millisecond), len(result.HTML))
	}
	f.applySession(ctx, target, result)
	return result.HTML, nil
}

func (f flaresolverrFetcher) applySession(ctx context.Context, target string, result browserfetch.Result) {
	if result.UserAgent != "" {
		f.state.SetUserAgent(result.UserAgent)
		if f.log != nil {
			f.log.Infof("Using FlareSolverr user-agent for follow-up requests.\n")
		}
	}
	if f.http == nil || f.http.Jar == nil || len(result.Cookies) == 0 {
		return
	}
	rawURL := result.URL
	if rawURL == "" {
		rawURL = target
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return
	}
	f.http.Jar.SetCookies(u, result.Cookies)
	if f.log != nil {
		f.log.Infof("Stored %d FlareSolverr cookies for follow-up requests.\n", len(result.Cookies))
	}
	if err := f.cache.save(ctx, rawURL, result.UserAgent, result.Cookies); err != nil {
		if f.log != nil {
			f.log.Debugf("Browser cookie cache save failed for %s: %v\n", rawURL, err)
		}
	} else if f.log != nil {
		f.log.Infof("Cached %d FlareSolverr cookies for future runs.\n", len(result.Cookies))
	}
}

// FetchChapters fetches and normalizes all chapters for a source URL. The
// second return is the URL the page resolved to (sites rotate slugs and
// redirect old ones); callers can persist it when it differs.
// languageGap reports chapters existing only outside the preferred languages
// after the last FetchChapters (LanguageAware scrapers only).
func (s *DownloadService) FetchChapters(ctx context.Context, sourceURL string) (list []chapters.Chapter, finalURL string, languageGap int, err error) {
	if s.cfg.Chapterless {
		return []chapters.Chapter{{Chapter: providers.Chapter{URL: sourceURL, Label: "Oneshot"}}}, sourceURL, 0, nil
	}
	var raw []providers.Chapter
	finalURL = sourceURL
	switch scraper := s.scraper.(type) {
	case providers.LanguageAware:
		raw, languageGap, err = scraper.GetChaptersByLanguage(ctx, sourceURL, s.cfg.Languages, s.cfg.LanguageMode == "all")
	case providers.RedirectAware:
		raw, finalURL, err = scraper.GetChaptersResolved(ctx, sourceURL)
	default:
		raw, err = s.scraper.GetChapters(ctx, sourceURL)
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("fetch chapters: %w", err)
	}

	out := make([]chapters.Chapter, len(raw))
	for i, ch := range raw {
		out[i] = chapters.Chapter{Chapter: ch}
	}
	return out, finalURL, languageGap, nil
}

// SelectChapters filters a chapter list according to a selection request.
func (s *DownloadService) SelectChapters(
	all []chapters.Chapter,
	selection ChapterSelection,
) ([]chapters.Chapter, error) {
	selected, err := chapters.Filter(
		all,
		selection.Chapter,
		selection.Range,
		selection.ExcludeRange,
		selection.List,
		selection.ExcludeList,
	)
	if err != nil {
		return nil, fmt.Errorf("select chapters: %w", err)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no chapters selected")
	}

	return selected, nil
}

// FetchImages fetches image URLs for a chapter.
func (s *DownloadService) FetchImages(ctx context.Context, chapter chapters.Chapter) ([]string, error) {
	images, err := s.scraper.GetImages(ctx, chapter.URL)
	if err != nil {
		return nil, fmt.Errorf("fetch images for %s: %w", chapter.Label, err)
	}

	return images, nil
}

// SearchManga uses the scraper's native search API when it has one. handled is
// false for scrapers that only support HTML search-page parsing.
func (s *DownloadService) SearchManga(ctx context.Context, searchURL, query string) (urls []string, handled bool, err error) {
	searcher, ok := s.scraper.(providers.Searcher)
	if !ok {
		return nil, false, nil
	}
	urls, err = searcher.SearchManga(ctx, searchURL, query)
	return urls, true, err
}

// VerifyImage downloads one image to a temporary folder to confirm the image
// host is actually reachable (catching CDN 403s that URL extraction misses).
func (s *DownloadService) VerifyImage(ctx context.Context, imageURL, referer string) error {
	tmp, err := os.MkdirTemp("", "kaodoku-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	files, _, err := s.downloader().DownloadImagesConcurrently(ctx, []string{imageURL}, tmp, referer, 1, s.progressHandle("verify"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("image not downloadable")
	}
	return nil
}

// Download downloads selected chapters and writes CBZ files.
func (s *DownloadService) Download(
	ctx context.Context,
	selected []chapters.Chapter,
) (DownloadSummary, error) {
	if len(selected) == 0 {
		return DownloadSummary{}, fmt.Errorf("no chapters selected")
	}

	start := time.Now()
	dl := s.downloader()

	var summary downloadCounters
	sem := make(chan struct{}, max(1, s.cfg.ChapterWorkers))
	var wg sync.WaitGroup

schedule:
	for _, ch := range selected {
		select {
		case <-ctx.Done():
			break schedule
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(ch chapters.Chapter) {
			defer wg.Done()
			defer func() { <-sem }()

			result, err := s.downloadChapter(ctx, dl, ch)
			if err != nil {
				msg := fmt.Sprintf("chapter %s: %v", ch.Label, err)
				s.log.Errorf("%s\n", msg)
				summary.addError(msg)
				summary.failedChapters.Add(1)
				return
			}
			summary.chapters.Add(1)
			summary.images.Add(int64(result.Images))
			summary.bytes.Add(result.Bytes)
		}(ch)
	}

	wg.Wait()
	if s.progress != nil {
		s.progress.Close()
	}

	if err := ctx.Err(); err != nil {
		return summary.toSummary(time.Since(start)), fmt.Errorf("download cancelled: %w", err)
	}

	return summary.toSummary(time.Since(start)), nil
}

// DownloadChapter downloads one chapter and writes its CBZ file.
func (s *DownloadService) DownloadChapter(ctx context.Context, ch chapters.Chapter) (ChapterDownloadResult, error) {
	return s.downloadChapter(ctx, s.downloader(), ch)
}

func (s *DownloadService) downloader() *downloader.Downloader {
	dl := downloader.New(s.client, s.cfg.SkipBroken)
	dl.SetBrowserFetcher(s.browser)
	return dl
}

func (s *DownloadService) downloadChapter(
	ctx context.Context,
	dl *downloader.Downloader,
	ch chapters.Chapter,
) (ChapterDownloadResult, error) {
	images, err := s.scraper.GetImages(ctx, ch.URL)
	if err != nil {
		return ChapterDownloadResult{}, fmt.Errorf("fetch images for %s: %w", ch.Label, err)
	}
	if len(images) == 0 {
		return ChapterDownloadResult{}, fmt.Errorf("no images for %s", ch.Label)
	}

	handle := s.progressHandle("Ch." + ch.Label)
	handle.SetTotal(len(images))

	tmpFolder := filepath.Join(s.cfg.Output, ch.FolderName())
	cbzOut := filepath.Join(s.cfg.Output, ch.OutputCBZ())
	if !s.cfg.KeepFolders {
		defer util.CleanupFolder(tmpFolder)
	}

	files, bytes, err := dl.DownloadImagesConcurrently(ctx, images, tmpFolder, ch.URL, max(1, s.cfg.ImageWorkers), handle)
	if err != nil {
		// Composite kotatsu chapter ids are not navigable pages so browser
		// fallback can only waste its timeout on them.
		if s.cfg.BrowserDownload.Enabled && !strings.Contains(ch.URL, "\t") {
			return s.downloadChapterWithBrowser(ctx, ch, cbzOut, tmpFolder, err)
		}
		return ChapterDownloadResult{}, err
	}

	extra := s.comicInfoEntry(ch, len(files))
	for name, body := range util.PageCropsEntry(files) {
		if extra == nil {
			extra = map[string][]byte{}
		}
		extra[name] = body
	}

	if err := util.CreateCBZ(files, cbzOut, s.cfg.SkipBroken, extra); err != nil {
		return ChapterDownloadResult{}, fmt.Errorf("create cbz: %w", err)
	}

	handle.MarkDone()
	return ChapterDownloadResult{Chapter: ch, OutputFile: cbzOut, Images: len(files), Bytes: bytes}, nil
}

// downloadChapterWithBrowser delegates CBZ assembly to the remote browser
// worker, so those archives carry no ComicInfo.xml.
func (s *DownloadService) downloadChapterWithBrowser(
	ctx context.Context,
	ch chapters.Chapter,
	cbzOut string,
	tmpFolder string,
	cause error,
) (ChapterDownloadResult, error) {
	if s.log != nil {
		s.log.Debugf("Chapter %s image download failed, trying browser downloader: %v\n", ch.Label, cause)
	}
	timeout := time.Duration(s.cfg.BrowserDownload.TimeoutSeconds) * time.Second
	client := browserdownload.New(s.cfg.BrowserDownload.Endpoint, timeout, nil)
	result, err := client.DownloadCBZ(ctx, browserdownload.Request{
		ChapterURL:        ch.URL,
		AllowedExtensions: s.cfg.AllowExt,
	}, cbzOut)
	if err != nil {
		return ChapterDownloadResult{}, fmt.Errorf("%w; browser downloader fallback failed: %v", cause, err)
	}
	return ChapterDownloadResult{Chapter: ch, OutputFile: cbzOut, Images: result.Images, Bytes: result.Bytes}, nil
}

func (s *DownloadService) progressHandle(prefix string) ProgressHandle {
	if s.progress == nil {
		return noopProgressHandle{}
	}

	return s.progress.Register(prefix)
}

type downloadCounters struct {
	chapters       atomic.Int64
	images         atomic.Int64
	bytes          atomic.Int64
	failedChapters atomic.Int64
	mu             sync.Mutex
	errors         []string
}

func (c *downloadCounters) toSummary(duration time.Duration) DownloadSummary {
	c.mu.Lock()
	errors := append([]string(nil), c.errors...)
	c.mu.Unlock()
	return DownloadSummary{
		Chapters:       c.chapters.Load(),
		Images:         c.images.Load(),
		Bytes:          c.bytes.Load(),
		FailedChapters: c.failedChapters.Load(),
		Errors:         errors,
		Duration:       duration,
	}
}

func (c *downloadCounters) addError(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errors = append(c.errors, msg)
}

type noopProgressHandle struct{}

func (noopProgressHandle) SetTotal(_ int) {}

func (noopProgressHandle) Update(_, _ int, _ int64) {}

func (noopProgressHandle) MarkDone() {}
