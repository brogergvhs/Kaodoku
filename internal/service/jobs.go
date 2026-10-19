package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/browserdownload"
	"github.com/brogergvhs/kaodoku/internal/browserfetch"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/ui"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// JobService enqueues and runs jobs.
type JobService struct {
	db         database.Handle
	dbPath     string
	secrets    *tokenCipher
	jobTimeout time.Duration
	jobWorkers int
	runtime    func() (*config.Config, ui.Log, error)
	jobs       *jobs.Repository
	lib        *LibraryService
	src        *sourceService
	want       *WantedService
	running    sync.Map // job ID -> context.CancelCauseFunc for in-flight jobs
	runMu      sync.Mutex
	rerun      atomic.Bool
	titleLocks sync.Map // title ID -> *sync.Mutex
	auth       *auth.Service
	recMu      sync.Mutex
	recCache   map[int64]cachedRecs // user ID -> recommendation grid
	adultMu    sync.Mutex
	adultTags  []string // vocabulary names flagged adult, briefly cached
	adultExp   time.Time
	kotMu      sync.Mutex
	kotSources []kotatsu.Source
	kotExp     time.Time
}

// cachedRecs is a user's recommendation pool, cached to spare AniList calls.
type cachedRecs struct {
	items   []catalog.Manga
	expires time.Time
}

const recommendationTTL = 10 * time.Minute

// errJobCancelled aborts an in-flight job on explicit user cancellation.
var errJobCancelled = errors.New("cancelled by user")

// JobPayload is the common payload for background jobs.
type JobPayload struct {
	TitleID              int64  `json:"title_id,omitempty"`
	SourceID             string `json:"source_id,omitempty"`
	CatalogID            int64  `json:"catalog_id,omitempty"`
	ResetFailed          bool   `json:"reset_failed,omitempty"`
	UserID               int64  `json:"user_id,omitempty"`
	DownloadAfterRefresh bool   `json:"download_after_refresh,omitempty"`
	Folder               string `json:"folder,omitempty"`
}

// RunSummary describes one queue drain.
type RunSummary struct {
	Done   int
	Failed int
}

const (
	SettingServeRefreshEvery      = "serve.refresh_every"
	SettingServeScanEvery         = "serve.scan_every"
	SettingServeDownloadEvery     = "serve.download_every"
	SettingServeSourceVerifyEvery = "sources.verify_every"
	SettingServeBackupEvery       = "backup.every"
	SettingServeRunEvery          = "serve.run_every"

	SettingBrowserSolverEnabled            = "browser_solver.enabled"
	SettingBrowserSolverProvider           = "browser_solver.provider"
	SettingBrowserSolverEndpoint           = "browser_solver.endpoint"
	SettingBrowserSolverTimeoutSeconds     = "browser_solver.timeout_seconds"
	SettingBrowserDownloaderEnabled        = "browser_downloader.enabled"
	SettingBrowserDownloaderEndpoint       = "browser_downloader.endpoint"
	SettingBrowserDownloaderTimeoutSeconds = "browser_downloader.timeout_seconds"
	SettingSourceRegistryURL               = "sources.registry_url"

	SettingJobsMaxAttempts      = "jobs.max_attempts"
	SettingJobsTimeout          = "jobs.timeout"
	SettingJobsWorkers          = "jobs.workers"
	SettingDownloadsMaxAttempts = "downloads.max_attempts"

	SettingServicesHealthInterval = "services.health_interval"

	SettingServeAniListSyncEvery = "sync.anilist_every"
	SettingServeCatalogEvery     = "catalog.refresh_every"
	SettingRateLimitIntervalMS   = "ratelimit.interval_ms"
	SettingRateLimitBurst        = "ratelimit.burst"
	SettingRateLimitDisabled     = "ratelimit.disabled"

	SettingAniListClientID     = "anilist.client_id"
	SettingAniListClientSecret = "anilist.client_secret"

	SettingUITheme        = "ui.theme"
	uiCustomColorPrefix   = "ui.custom."
	SettingDefaultUITheme = "mocha"

	defaultJobTimeout = 10 * time.Minute
	defaultJobWorkers = 4
)

// SettingDefault returns the built-in value for an app setting.
func SettingDefault(key string) string {
	switch key {
	case SettingServeRefreshEvery:
		return "1h"
	case SettingServeScanEvery:
		return "30m"
	case SettingServeDownloadEvery:
		return "10m"
	case SettingServeSourceVerifyEvery:
		return "168h"
	case SettingServeBackupEvery:
		return ""
	case SettingServeRunEvery:
		return "5s"
	case SettingBrowserSolverEnabled:
		return "false"
	case SettingBrowserSolverProvider:
		return browserfetch.ProviderFlareSolverr
	case SettingBrowserSolverEndpoint:
		return browserfetch.DefaultFlareSolverrEndpoint
	case SettingBrowserSolverTimeoutSeconds:
		return "60"
	case SettingBrowserDownloaderEnabled:
		return "false"
	case SettingBrowserDownloaderEndpoint:
		return browserdownload.DefaultEndpoint
	case SettingBrowserDownloaderTimeoutSeconds:
		return "180"
	case SettingSourceRegistryURL:
		return ""
	case SettingJobsMaxAttempts, SettingDownloadsMaxAttempts:
		return "3"
	case SettingJobsTimeout:
		return "10m"
	case SettingJobsWorkers:
		return "4"
	case SettingServicesHealthInterval:
		return "60s"
	case SettingServeAniListSyncEvery:
		return "" // disabled until a cadence is set
	case SettingServeCatalogEvery:
		return "168h" // stale release status would block AniList Completed pushes
	case SettingRateLimitIntervalMS:
		return "200"
	case SettingRateLimitBurst:
		return "2"
	case SettingRateLimitDisabled:
		return "false"
	case SettingUITheme:
		return SettingDefaultUITheme
	default:
		if c, ok := strings.CutPrefix(key, uiCustomColorPrefix); ok {
			return mochaColors[c]
		}
		return ""
	}
}

// UIThemes lists the selectable interface themes.
func UIThemes() []string {
	return []string{"mocha", "latte", "dracula", "nord", "custom"}
}

// CustomColorTokens lists the theme color variables a custom theme fills.
func CustomColorTokens() []string {
	return []string{
		"base-100", "base-200", "base-300", "base-content",
		"primary", "primary-content", "secondary", "neutral",
		"line", "muted", "info", "success", "warning", "error",
	}
}

// CustomColorKey maps a color token to its setting key.
func CustomColorKey(token string) string { return uiCustomColorPrefix + token }

// mochaColors seeds the custom-theme editor with the default palette.
var mochaColors = map[string]string{
	"base-100": "#1e1e2e", "base-200": "#181825", "base-300": "#11111b", "base-content": "#cdd6f4",
	"primary": "#cba6f7", "primary-content": "#11111b", "secondary": "#89b4fa", "neutral": "#313244",
	"line": "#45475a", "muted": "#a6adc8", "info": "#89dceb", "success": "#a6e3a1",
	"warning": "#f9e2af", "error": "#f38ba8",
}

var hexColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// SettingKeys returns settings exposed through the API.
func SettingKeys() []string {
	return []string{
		SettingServeRefreshEvery,
		SettingServeScanEvery,
		SettingServeDownloadEvery,
		SettingServeSourceVerifyEvery,
		SettingServeBackupEvery,
		SettingServeRunEvery,
		SettingBrowserSolverEnabled,
		SettingBrowserSolverProvider,
		SettingBrowserSolverEndpoint,
		SettingBrowserSolverTimeoutSeconds,
		SettingBrowserDownloaderEnabled,
		SettingBrowserDownloaderEndpoint,
		SettingBrowserDownloaderTimeoutSeconds,
		SettingSourceRegistryURL,
		SettingJobsMaxAttempts,
		SettingJobsTimeout,
		SettingJobsWorkers,
		SettingDownloadsMaxAttempts,
		SettingServicesHealthInterval,
		SettingServeAniListSyncEvery,
		SettingServeCatalogEvery,
		SettingRateLimitIntervalMS,
		SettingRateLimitBurst,
		SettingRateLimitDisabled,
		SettingAniListClientID,
		SettingAniListClientSecret,
		SettingUITheme,
		CustomColorKey("base-100"), CustomColorKey("base-200"), CustomColorKey("base-300"), CustomColorKey("base-content"),
		CustomColorKey("primary"), CustomColorKey("primary-content"), CustomColorKey("secondary"), CustomColorKey("neutral"),
		CustomColorKey("line"), CustomColorKey("muted"), CustomColorKey("info"), CustomColorKey("success"),
		CustomColorKey("warning"), CustomColorKey("error"),
	}
}

// ValidateSetting checks an app setting update.
func ValidateSetting(key, value string) error {
	if !isSettingKey(key) {
		return fmt.Errorf("unknown setting %q", key)
	}

	if strings.HasPrefix(key, uiCustomColorPrefix) {
		if !hexColorRe.MatchString(value) {
			return fmt.Errorf("invalid color for %s: use #rrggbb", key)
		}
		return nil
	}
	switch key {
	case SettingUITheme:
		for _, t := range UIThemes() {
			if value == t {
				return nil
			}
		}
		return fmt.Errorf("unknown theme %q", value)
	case SettingServeAniListSyncEvery, SettingServeCatalogEvery, SettingServeBackupEvery:
		if value == "" {
			return nil
		}
		return validateDurationSetting(key, value)
	case SettingServeRefreshEvery, SettingServeScanEvery, SettingServeDownloadEvery, SettingServeSourceVerifyEvery, SettingServeRunEvery, SettingServicesHealthInterval:
		return validateDurationSetting(key, value)
	case SettingBrowserSolverEnabled, SettingBrowserDownloaderEnabled, SettingRateLimitDisabled:
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("invalid bool for %s", key)
		}
	case SettingRateLimitIntervalMS, SettingRateLimitBurst:
		if n, err := strconv.Atoi(value); err != nil || n < 0 {
			return fmt.Errorf("%s must be a non-negative number", key)
		}
	case SettingBrowserSolverProvider:
		if value != browserfetch.ProviderFlareSolverr {
			return fmt.Errorf("unsupported provider %q", value)
		}
	case SettingBrowserSolverEndpoint, SettingBrowserDownloaderEndpoint:
		u, err := url.ParseRequestURI(value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid endpoint for %s", key)
		}
	case SettingBrowserSolverTimeoutSeconds, SettingBrowserDownloaderTimeoutSeconds:
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds <= 0 {
			return fmt.Errorf("invalid timeout seconds for %s", key)
		}
	case SettingSourceRegistryURL:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		u, err := url.ParseRequestURI(value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid registry url for %s", key)
		}
	case SettingJobsMaxAttempts, SettingDownloadsMaxAttempts, SettingJobsWorkers:
		if n, err := strconv.Atoi(value); err != nil || n <= 0 {
			return fmt.Errorf("invalid positive integer for %s", key)
		}
	case SettingJobsTimeout:
		if d, err := time.ParseDuration(value); err != nil || d <= 0 {
			return fmt.Errorf("invalid positive duration for %s", key)
		}
	}
	return nil
}

func isSettingKey(key string) bool {
	for _, known := range SettingKeys() {
		if key == known {
			return true
		}
	}
	return false
}

func validateDurationSetting(key, value string) error {
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return fmt.Errorf("invalid duration for %s", key)
	}
	if key == SettingServeRunEvery && d == 0 {
		return fmt.Errorf("%s cannot be 0", key)
	}
	return nil
}

// OpenJobs opens the app database for job processing.
func OpenJobs(ctx context.Context, dbPath string) (*JobService, func(), error) {
	if dbPath == "" {
		dbPath = database.DefaultPath()
	}
	w, err := database.Open(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := database.Migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, nil, err
	}
	db, err := database.WithReaders(ctx, w, dbPath)
	if err != nil {
		_ = w.Close()
		return nil, nil, err
	}

	svc := newJobService(db)
	svc.dbPath = dbPath
	if svc.secrets, err = newTokenCipher(dbPath); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if err := svc.encryptLegacyTokens(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	svc.auth = auth.NewService(db)
	if err := svc.auth.Bootstrap(ctx, os.Getenv("KAODOKU_ADMIN_USER"), os.Getenv("KAODOKU_ADMIN_PASSWORD")); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if err := svc.auth.PurgeExpiredSessions(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if err := svc.auth.PurgeExpiredAPITokens(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if _, err := svc.lib.ReconcileStartedDownloads(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if _, err := svc.jobs.ReconcileRunning(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	svc.applyLimits(ctx)
	go func() {
		if _, err := svc.lib.ScanDownloads(ctx, nil, 0); err != nil && ctx.Err() == nil {
			log.Printf("startup download stats scan failed: %v", err)
		}
	}()
	go func() {
		if tags, err := svc.want.catalog.ContentTags(ctx); err == nil && !hasAdultTag(tags) {
			if err := svc.refreshTagVocabulary(ctx); err != nil && ctx.Err() == nil {
				log.Printf("startup tag vocabulary refresh failed: %v", err)
			}
		}
	}()
	return svc, func() { _ = db.Close() }, nil
}

func newJobService(db database.Handle) *JobService {
	svc := &JobService{
		db:         db,
		dbPath:     database.DefaultPath(),
		jobTimeout: defaultJobTimeout,
		jobWorkers: defaultJobWorkers,
		runtime: func() (*config.Config, ui.Log, error) {
			return config.DefaultConfig(), ui.NewLogger(false), nil
		},
		jobs: jobs.NewRepository(db),
		lib:  newLibraryService(db),
		src:  newSourceService(db),
		want: newWantedService(db),
	}
	svc.lib.SetMangaLookup(svc.want.GetManga)
	return svc
}

// applyLimits seeds the retry/timeout tunables from stored settings.
func (s *JobService) applyLimits(ctx context.Context) {
	if n, err := strconv.Atoi(s.Setting(ctx, SettingJobsMaxAttempts, SettingDefault(SettingJobsMaxAttempts))); err == nil && n > 0 {
		s.jobs.MaxAttempts = n
	}
	if n, err := strconv.Atoi(s.Setting(ctx, SettingDownloadsMaxAttempts, SettingDefault(SettingDownloadsMaxAttempts))); err == nil && n > 0 {
		s.lib.repo.MaxDownloadAttempts = n
	}
	if d, err := time.ParseDuration(s.Setting(ctx, SettingJobsTimeout, SettingDefault(SettingJobsTimeout))); err == nil && d > 0 {
		s.jobTimeout = d
	}
	if n, err := strconv.Atoi(s.Setting(ctx, SettingJobsWorkers, SettingDefault(SettingJobsWorkers))); err == nil && n > 0 {
		s.jobWorkers = n
	}
}

// SetRuntimeConfig overrides how job execution loads the base runtime config
// (e.g. the CLI's merged config file); DB-backed settings still overlay it.
func (s *JobService) SetRuntimeConfig(fn func() (*config.Config, ui.Log, error)) {
	if fn != nil {
		s.runtime = fn
	}
}

// RuntimeConfig returns the merged runtime config for job execution.
func (s *JobService) RuntimeConfig(ctx context.Context) (*config.Config, ui.Log, error) {
	cfg, logSvc, err := s.runtime()
	if err != nil {
		return nil, nil, err
	}
	s.ApplySettings(ctx, cfg)
	cfg.CookieDBPath = s.dbPath
	return cfg, logSvc, nil
}

// Setting returns an app setting, or fallback when the key is unset.
func (s *JobService) Setting(ctx context.Context, key, fallback string) string {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("read setting %s: %v (using fallback)", key, err)
		}
		return fallback
	}
	return value
}

// LastJobTime returns when a job of the given type was last enqueued, or the
// zero time when none exists.
func (s *JobService) LastJobTime(ctx context.Context, typ string) time.Time {
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM jobs WHERE type = ?`, typ).Scan(&raw); err != nil || !raw.Valid {
		return time.Time{}
	}
	t, err := database.ParseTime(raw.String)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Auth exposes the user/role/session service.
func (s *JobService) Auth() *auth.Service { return s.auth }

// UserSettings returns a user's personal settings (e.g. appearance).
func (s *JobService) UserSettings(ctx context.Context, userID int64) map[string]string {
	out := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM user_settings WHERE user_id = ?`, userID)
	if err != nil {
		log.Printf("read user settings: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// SetUserSetting stores a personal setting; empty value clears it.
func (s *JobService) SetUserSetting(ctx context.Context, userID int64, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM user_settings WHERE user_id = ? AND key = ?`, userID, key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value
	`, userID, key, value)
	return err
}

// AllSettings returns every stored setting in one query.
func (s *JobService) AllSettings(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		log.Printf("read settings: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// SetSetting stores an app setting.
func (s *JobService) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// ClearSetting removes a stored override so the config/env default applies.
func (s *JobService) ClearSetting(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("clear setting %s: %w", key, err)
	}
	return nil
}

// ApplySettings overlays DB-backed app settings onto cfg.
func (s *JobService) ApplySettings(ctx context.Context, cfg *config.Config) {
	if value := s.Setting(ctx, SettingBrowserSolverEnabled, ""); value != "" {
		if enabled, err := strconv.ParseBool(value); err == nil {
			cfg.BrowserSolver.Enabled = enabled
		}
	}
	if value := s.Setting(ctx, SettingBrowserSolverProvider, ""); value != "" {
		cfg.BrowserSolver.Provider = value
	}
	if value := s.Setting(ctx, SettingBrowserSolverEndpoint, ""); value != "" {
		cfg.BrowserSolver.Endpoint = value
	}
	if value := s.Setting(ctx, SettingBrowserSolverTimeoutSeconds, ""); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			cfg.BrowserSolver.TimeoutSeconds = seconds
		}
	}
	if value := s.Setting(ctx, SettingBrowserDownloaderEnabled, ""); value != "" {
		if enabled, err := strconv.ParseBool(value); err == nil {
			cfg.BrowserDownload.Enabled = enabled
		}
	}
	if value := s.Setting(ctx, SettingBrowserDownloaderEndpoint, ""); value != "" {
		cfg.BrowserDownload.Endpoint = value
	}
	if value := s.Setting(ctx, SettingBrowserDownloaderTimeoutSeconds, ""); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			cfg.BrowserDownload.TimeoutSeconds = seconds
		}
	}
	if value := s.Setting(ctx, SettingRateLimitIntervalMS, ""); value != "" {
		if ms, err := strconv.Atoi(value); err == nil {
			cfg.RateLimit.IntervalMS = ms
		}
	}
	if value := s.Setting(ctx, SettingRateLimitBurst, ""); value != "" {
		if burst, err := strconv.Atoi(value); err == nil {
			cfg.RateLimit.Burst = burst
		}
	}
	if value := s.Setting(ctx, SettingRateLimitDisabled, ""); value != "" {
		if disabled, err := strconv.ParseBool(value); err == nil {
			cfg.RateLimit.Disabled = disabled
		}
	}
}

// BrowserSolverHealth checks the configured browser solver endpoint.
func (s *JobService) BrowserSolverHealth(ctx context.Context) (bool, error) {
	enabled, _ := strconv.ParseBool(s.Setting(ctx, SettingBrowserSolverEnabled, SettingDefault(SettingBrowserSolverEnabled)))
	if !enabled {
		return false, nil
	}
	if provider := s.Setting(ctx, SettingBrowserSolverProvider, SettingDefault(SettingBrowserSolverProvider)); provider != browserfetch.ProviderFlareSolverr {
		return false, fmt.Errorf("unsupported browser solver provider %q", provider)
	}

	timeoutSeconds, _ := strconv.Atoi(s.Setting(ctx, SettingBrowserSolverTimeoutSeconds, SettingDefault(SettingBrowserSolverTimeoutSeconds)))
	client := browserfetch.NewFlareSolverr(
		s.Setting(ctx, SettingBrowserSolverEndpoint, SettingDefault(SettingBrowserSolverEndpoint)),
		time.Duration(timeoutSeconds)*time.Second,
		nil,
	)
	if err := client.Health(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ListTitles returns tracked titles.
func (s *JobService) ListTitles(ctx context.Context) ([]library.Title, error) {
	return s.lib.ListTitles(ctx)
}

// GetTitle returns one tracked title.
func (s *JobService) GetTitle(ctx context.Context, id int64) (library.Title, error) {
	return s.lib.GetTitle(ctx, id)
}

// TitleReadStatuses returns all discovered chapters with download + read state.
func (s *JobService) TitleReadStatuses(ctx context.Context, id int64) ([]library.ChapterReadStatus, error) {
	return s.lib.TitleReadStatuses(ctx, id)
}

// TitleChapters returns all discovered chapters for a title with download state.
func (s *JobService) TitleChapters(ctx context.Context, id int64) ([]library.ChapterStatus, error) {
	return s.lib.ListChapters(ctx, id)
}

// ReaderProgress returns downloaded chapters and read state for a title.
func (s *JobService) ReaderProgress(ctx context.Context, id int64) (library.TitleReadProgress, error) {
	return s.lib.ReaderProgress(ctx, id)
}

// ChapterReadStatus returns reader/download state for one chapter.
func (s *JobService) ChapterReadStatus(ctx context.Context, chapterID int64) (library.ChapterReadStatus, error) {
	return s.lib.ChapterReadStatus(ctx, chapterID)
}

// MarkPageRead records one completed page for reader resume/progress.
func (s *JobService) MarkPageRead(ctx context.Context, chapterID int64, page, totalPages int) (library.ChapterReadStatus, error) {
	return s.lib.MarkPageRead(ctx, chapterID, page, totalPages)
}

// MarkChapterRead records a completed chapter.
func (s *JobService) MarkChapterRead(ctx context.Context, chapterID int64) (library.ChapterReadStatus, error) {
	return s.lib.MarkChapterRead(ctx, chapterID)
}

// MarkChapterUnread clears read progress for a chapter.
func (s *JobService) MarkChapterUnread(ctx context.Context, chapterID int64) (library.ChapterReadStatus, error) {
	return s.lib.MarkChapterUnread(ctx, chapterID)
}

// MarkChapterRangeRead marks downloaded chapters in a title range read.
func (s *JobService) MarkChapterRangeRead(ctx context.Context, titleID int64, from, to string) (int, error) {
	return s.lib.MarkChapterRangeRead(ctx, titleID, from, to)
}

// MarkChapterRangeUnread clears read progress in a title range.
func (s *JobService) MarkChapterRangeUnread(ctx context.Context, titleID int64, from, to string) (int, error) {
	return s.lib.MarkChapterRangeUnread(ctx, titleID, from, to)
}

// RemoveChapterDownload deletes a downloaded chapter from disk; it becomes missing.
func (s *JobService) RemoveChapterDownload(ctx context.Context, chapterID int64) error {
	return s.lib.RemoveChapterDownload(ctx, chapterID)
}

// RenameChapter updates a chapter's descriptive title.
func (s *JobService) RenameChapter(ctx context.Context, chapterID int64, title string) error {
	return s.lib.RenameChapter(ctx, chapterID, title)
}

// ResetChapterTitle restores one manually renamed chapter's scraped name.
func (s *JobService) ResetChapterTitle(ctx context.Context, chapterID int64) error {
	return s.lib.ResetChapterTitle(ctx, chapterID)
}

// BulkRenameChapters applies manual names and volume assignments atomically.
func (s *JobService) BulkRenameChapters(ctx context.Context, titleID int64, items []library.ChapterRename) error {
	return s.lib.BulkRenameChapters(ctx, titleID, items)
}

// ResetChapterTitles restores scraped names for the whole title or a range.
func (s *JobService) ResetChapterTitles(ctx context.Context, titleID int64, from, to string) (int, error) {
	return s.lib.ResetChapterTitles(ctx, titleID, from, to)
}

// RemoveChapterDownloadsRange deletes every downloaded chapter whose whole
// number falls in [from, to]; returns how many were removed.
func (s *JobService) RemoveChapterDownloadsRange(ctx context.Context, titleID int64, from, to string) (int, error) {
	f, ferr := strconv.Atoi(strings.TrimSpace(from))
	t, terr := strconv.Atoi(strings.TrimSpace(to))
	if ferr != nil || terr != nil {
		return 0, fmt.Errorf("whole chapter numbers are required")
	}
	if f > t {
		f, t = t, f
	}
	chs, err := s.TitleChapters(ctx, titleID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, c := range chs {
		if c.Downloaded && c.NumberMain >= f && c.NumberMain <= t {
			if err := s.lib.RemoveChapterDownload(ctx, c.ID); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// ListTitleSources returns all sources linked to a title.
func (s *JobService) ListTitleSources(ctx context.Context, id int64) ([]library.LinkedSource, error) {
	return s.lib.ListTitleSources(ctx, id)
}

func (s *JobService) SourceTitleIDs(ctx context.Context, sourceID string) (map[int64]bool, error) {
	return s.lib.TitleIDsForSource(ctx, sourceID)
}

// UnlinkTitleSource removes a linked source from a title.
func (s *JobService) UnlinkTitleSource(ctx context.Context, id int64, url string) error {
	return s.lib.UnlinkSource(ctx, id, url)
}

// AddCatalogTitle adds an AniList manga to the library as a source-less title.
func (s *JobService) AddCatalogTitle(ctx context.Context, anilistID int) (library.Title, error) {
	title, err := s.want.AddCatalogTitle(ctx, anilistID)
	if err != nil {
		return library.Title{}, err
	}
	s.PushAniListEntry(ctx, auth.UserID(ctx), title.ID)
	if err := s.enqueueSourceSearchForTitle(ctx, title); err != nil {
		return title, err
	}
	return title, nil
}

// TitlesByProvider maps a catalog provider's manga IDs to tracked title IDs.
func (s *JobService) TitlesByProvider(ctx context.Context, provider string) (map[string]int64, error) {
	return s.lib.TitlesByProvider(ctx, provider)
}

// GetManga returns stored catalog metadata for a manga.

// AniListConnection describes a user's linked AniList account.
type AniListConnection struct {
	Connected    bool
	Name         string
	ExpiresAt    string
	ExpiringSoon bool // less than 30 days of token validity left
}

// AniListConnectionFor returns the user's AniList link state.
func (s *JobService) AniListConnectionFor(ctx context.Context, userID int64) AniListConnection {
	var name, expires string
	err := s.db.QueryRowContext(ctx, `SELECT anilist_name, expires_at FROM user_anilist WHERE user_id = ?`, userID).Scan(&name, &expires)
	if err != nil {
		return AniListConnection{}
	}
	conn := AniListConnection{Connected: true, Name: name, ExpiresAt: expires}
	if t, err := database.ParseTime(expires); err == nil && expires != "" {
		conn.ExpiringSoon = time.Until(t) < 30*24*time.Hour
	}
	return conn
}

// ConnectAniList exchanges an OAuth code and stores the user's token.
func (s *JobService) ConnectAniList(ctx context.Context, userID int64, redirectURI, code string) error {
	clientID := s.Setting(ctx, SettingAniListClientID, "")
	secret := s.Setting(ctx, SettingAniListClientSecret, "")
	if clientID == "" || secret == "" {
		return fmt.Errorf("the AniList application is not configured (client id/secret in settings)")
	}
	token, expiresIn, err := catalog.AniListExchangeCode(ctx, clientID, secret, redirectURI, code)
	if err != nil {
		return err
	}
	aid, name, err := catalog.AniListViewer(ctx, token)
	if err != nil {
		return err
	}
	expires := ""
	if expiresIn > 0 {
		expires = database.FormatTime(time.Now().Add(time.Duration(expiresIn) * time.Second))
	}
	token, err = s.secrets.Encrypt(token)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO user_anilist (user_id, access_token, anilist_user_id, anilist_name, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			access_token = excluded.access_token,
			anilist_user_id = excluded.anilist_user_id,
			anilist_name = excluded.anilist_name,
			expires_at = excluded.expires_at
	`, userID, token, aid, name, expires)
	if err != nil {
		return err
	}
	s.invalidateRecs(userID)
	return nil
}

// DisconnectAniList removes the user's stored token.
func (s *JobService) DisconnectAniList(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_anilist WHERE user_id = ?`, userID); err != nil {
		return err
	}
	s.invalidateRecs(userID)
	return nil
}

// expandAniListSync spawns one child sync job per connected user.
func (s *JobService) expandAniListSync(ctx context.Context, parentID int64) error {
	// Collect-then-enqueue: an open rows cursor holds SQLite's only connection.
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM user_anilist`)
	if err != nil {
		return err
	}
	var userIDs []int64
	for rows.Next() {
		var uid int64
		if rows.Scan(&uid) == nil {
			userIDs = append(userIDs, uid)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var errs []error
	for _, uid := range userIDs {
		_, err := s.enqueueChild(ctx, jobs.TypeSyncAniList, JobPayload{UserID: uid}, time.Now(), parentID)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// runAniListSync reconciles the whole library with AniList for one user:
// remote > local pulls read marks down; local > remote pushes up; tracked
// titles missing from the remote list are added with their computed status.
func (s *JobService) runAniListSync(ctx context.Context, userID int64, progress ProgressManager) error {
	user, err := s.auth.GetUser(ctx, userID)
	if err != nil || user == nil {
		user = &auth.User{ID: userID}
	}
	ctx = auth.WithUser(ctx, user)
	actx, aid, ok := s.aniListIdentity(ctx, userID)
	if !ok {
		return nil // disconnected since the sweep expanded
	}
	entries, err := s.want.AniList().UserList(actx, aid)
	if err != nil {
		return s.aniListAuthError(ctx, userID, err)
	}
	remote := make(map[string]catalog.AniListEntry, len(entries))
	for _, e := range entries {
		remote[e.Manga.ProviderID] = e
	}
	tracked, err := s.lib.TitlesByProvider(ctx, catalog.AniListProvider)
	if err != nil {
		return err
	}
	titles, err := s.lib.ListTitles(ctx)
	if err != nil {
		return err
	}
	byID := make(map[int64]library.Title, len(titles))
	for _, t := range titles {
		byID[t.ID] = t
	}
	owners, err := s.lib.TitleOwners(ctx)
	if err != nil {
		return err
	}
	remoteFav := map[string]bool{}
	favFetched := false
	if ids, err := s.want.AniList().FavouriteManga(actx, aid); err == nil {
		favFetched = true
		for _, id := range ids {
			remoteFav[strconv.Itoa(id)] = true
		}
	}
	localFav, err := s.localFavouriteIDs(ctx, userID)
	if err != nil {
		return err
	}
	// Rate-limit pacing makes a large first sync slow; report each title so
	// the stall watchdog sees a working job, not a stuck one.
	handle := progress.Register("anilist sync")
	handle.SetTotal(len(tracked))
	defer handle.MarkDone()
	var errs []error
	n := 0
	for pid, titleID := range tracked {
		n++
		handle.Update(n, len(tracked), 0)
		mediaID, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		t := byID[titleID]
		if !contentAllowedFor(user, t.IsAdult, t.ContentTags) {
			continue
		}
		if remoteFav[pid] && !localFav[pid] {
			if err := s.lib.SetFavourite(ctx, userID, titleID); err != nil {
				errs = append(errs, err)
			}
		} else if favFetched && localFav[pid] && !remoteFav[pid] {
			if err := s.want.AniList().ToggleFavourite(actx, mediaID); err != nil {
				errs = append(errs, fmt.Errorf("favourite %s: %w", pid, err))
			}
		}
		e, listed := remote[pid]
		if listed && e.Progress > 0 {
			local, _, err := s.localAniListState(ctx, userID, titleID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if e.Progress > local {
				if _, err := s.lib.MarkChaptersReadThrough(ctx, titleID, e.Progress); err != nil {
					errs = append(errs, fmt.Errorf("pull %s: %w", pid, err))
					continue
				}
			}
		}
		local, status, err := s.localAniListState(ctx, userID, titleID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !listed {
			if status == "PLANNING" && !ownsTitle(userID, owners[titleID]) {
				continue
			}
			if err := s.want.AniList().SaveEntry(actx, mediaID, local, status); err != nil {
				errs = append(errs, fmt.Errorf("add %s: %w", pid, err))
			}
			continue
		}
		progress := -1
		if local > e.Progress {
			progress = local
		}
		push := ""
		if aniListStatusRank(status) > aniListStatusRank(e.Status) {
			push = status
		}
		if progress >= 0 || push != "" {
			if err := s.want.AniList().SaveEntry(actx, mediaID, progress, push); err != nil {
				errs = append(errs, fmt.Errorf("push %s: %w", pid, err))
			}
		}
	}
	s.invalidateRecs(userID)
	return s.aniListAuthError(ctx, userID, errs2err(errs))
}

func (s *JobService) aniListAuthError(ctx context.Context, userID int64, err error) error {
	if !catalog.IsUnauthorized(err) {
		return err
	}
	if derr := s.DisconnectAniList(ctx, userID); derr != nil {
		return errors.Join(fmt.Errorf("anilist authorization expired; reconnect AniList in settings"), derr)
	}
	return fmt.Errorf("anilist authorization expired; reconnect AniList in settings")
}

// contentAllowedFor mirrors the server's per-user content guard so AniList
// sync never surfaces a title the user cannot view in the app.
func contentAllowedFor(u *auth.User, isAdult bool, tags []string) bool {
	if u == nil {
		return false
	}
	if isAdult && !u.AllowAdult {
		return false
	}
	if len(u.AllowedTags) > 0 && !library.HasAnyTag(tags, u.AllowedTags) {
		return false
	}
	return !library.HasAnyTag(tags, u.BlockedTags)
}

// ownsTitle reports whether the user added the title. Titles added before
// ownership was tracked (added_by 0) belong to the env admin.
func ownsTitle(userID, addedBy int64) bool {
	return addedBy == userID || (addedBy == 0 && userID == auth.EnvAdminID)
}

// localAniListState computes the user's progress and list status for a title:
// nothing read → PLANNING; everything read AND the series finished releasing
// → COMPLETED (a caught-up ongoing series stays CURRENT); otherwise CURRENT.
// Status keys off the read count, not the numeric progress: sub-chapters like
// 0.1–0.3 carry progress 0 on AniList but still mean the user is reading.
func (s *JobService) localAniListState(ctx context.Context, userID, titleID int64) (int, string, error) {
	var progress, read, total int
	var release string
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(MAX(CASE WHEN rp.completed = 1 THEN c.number_main END), 0),
			COUNT(CASE WHEN rp.completed = 1 THEN 1 END),
			COUNT(*),
			COALESCE((SELECT cm.status FROM titles t LEFT JOIN catalog_manga cm ON cm.id = t.catalog_manga_id WHERE t.id = ?), '')
		FROM chapters c
		LEFT JOIN chapter_read_progress rp ON rp.chapter_id = c.id AND rp.user_id = ?
		WHERE c.title_id = ?`, titleID, userID, titleID).Scan(&progress, &read, &total, &release); err != nil {
		return 0, "", err
	}
	switch {
	case read == 0:
		return progress, "PLANNING", nil
	case read >= total && total > 0 && releaseFinished(release):
		return progress, "COMPLETED", nil
	default:
		return progress, "CURRENT", nil
	}
}

// releaseFinished mirrors defaultMonitored's reading of catalog status.
func releaseFinished(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "finished", "completed", "complete":
		return true
	default:
		return false
	}
}

// aniListStatusRank orders statuses so pushes only ever upgrade. DROPPED is
// overridable (a re-added title is no longer dropped); PAUSED and REPEATING
// are manual reading states the sync must not touch.
func aniListStatusRank(status string) int {
	switch status {
	case "", "DROPPED":
		return 0
	case "PLANNING":
		return 1
	case "CURRENT":
		return 2
	case "COMPLETED":
		return 3
	default: // PAUSED, REPEATING
		return 9
	}
}

func errs2err(errs []error) error { return errors.Join(errs...) }

// aniListIdentity returns the acting user's connection (token ctx + anilist id).
func (s *JobService) aniListIdentity(ctx context.Context, userID int64) (context.Context, int, bool) {
	var token string
	var aid int
	if err := s.db.QueryRowContext(ctx, `SELECT access_token, anilist_user_id FROM user_anilist WHERE user_id = ?`, userID).Scan(&token, &aid); err != nil || token == "" {
		return ctx, 0, false
	}
	token, err := s.secrets.Decrypt(token)
	if err != nil {
		log.Printf("anilist token decrypt failed for user %d: %v", userID, err)
		return ctx, 0, false
	}
	if token == "" {
		return ctx, 0, false
	}
	return catalog.WithToken(ctx, token), aid, true
}

// aniListConnected reports whether a token row exists at all, separating "not
// connected" from "connected but the token can't be decrypted".
func (s *JobService) aniListConnected(ctx context.Context, userID int64) bool {
	var one int
	return s.db.QueryRowContext(ctx,
		`SELECT 1 FROM user_anilist WHERE user_id = ? AND access_token != ''`, userID).Scan(&one) == nil
}

// aniListError explains why aniListIdentity failed, for user-facing messages.
func (s *JobService) aniListError(ctx context.Context, userID int64) error {
	if s.aniListConnected(ctx, userID) {
		return fmt.Errorf("AniList token couldn't be read — reconnect AniList (the server's encryption key changed)")
	}
	return fmt.Errorf("no AniList account connected")
}

// runCatalogRefresh re-fetches AniList metadata for every catalog entry a
// tracked title links to: descriptions, tags, adult flags and — importantly —
// the release status that gates the AniList Completed push all go stale
// otherwise. Paced by the AniList client's rate limiter.
func (s *JobService) runCatalogRefresh(ctx context.Context, progress ProgressManager) error {
	var errs []error
	if err := s.refreshTagVocabulary(ctx); err != nil {
		errs = append(errs, err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT cm.provider_id
		FROM titles t JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE cm.provider = ?`, catalog.AniListProvider)
	if err != nil {
		return err
	}
	var pids []string
	for rows.Next() {
		var pid string
		if rows.Scan(&pid) == nil && pid != "" {
			pids = append(pids, pid)
		}
	}
	rows.Close()
	handle := progress.Register("catalog refresh")
	handle.SetTotal(len(pids))
	defer handle.MarkDone()
	for i, pid := range pids {
		handle.Update(i+1, len(pids), 0)
		mediaID, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		if err := s.want.RefreshManga(ctx, mediaID); err != nil && !catalog.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("refresh %s: %w", pid, err))
		}
	}
	return errs2err(errs)
}

func hasAdultTag(tags []catalog.ContentTag) bool {
	for _, t := range tags {
		if t.IsAdult {
			return true
		}
	}
	return false
}

// refreshTagVocabulary re-fetches AniList's global genre/tag lists.
func (s *JobService) refreshTagVocabulary(ctx context.Context) error {
	genres, tags, err := s.want.AniList().TagVocabulary(ctx)
	if err != nil {
		return fmt.Errorf("tag vocabulary: %w", err)
	}
	if len(genres) == 0 && len(tags) == 0 {
		return fmt.Errorf("tag vocabulary: anilist returned nothing; keeping the stored one")
	}
	if err := s.want.catalog.ReplaceContentTags(ctx, genres, tags); err != nil {
		return err
	}
	s.adultMu.Lock()
	s.adultExp = time.Time{}
	s.adultMu.Unlock()
	return nil
}

// StoredContentTags returns the stored vocabulary without remote fetching.
func (s *JobService) StoredContentTags(ctx context.Context) ([]catalog.ContentTag, error) {
	return s.want.catalog.ContentTags(ctx)
}

// seedAdultTags guards non-adult users before the live vocabulary loads or when
// AniList is unreachable; mirrors MediaTagCollection.isAdult.
var seedAdultTags = []string{
	"Hentai",
	"Ahegao", "Amputation", "Anal Sex", "Armpits", "Ashikoki", "Asphyxiation",
	"Bondage", "Boobjob", "Cervix Penetration", "Cheating", "Cumflation",
	"Cunnilingus", "DILF", "Deepthroat", "Defloration", "Double Penetration",
	"Erotic Piercings", "Exhibitionism", "Facial", "Feet", "Fellatio", "Femdom",
	"Fingering", "Fisting", "Flat Chest", "Futanari", "Group Sex", "Hair Pulling",
	"Handjob", "Human Pet", "Hypersexuality", "Incest", "Inseki", "Irrumatio",
	"Lactation", "Large Breasts", "MILF", "Male Pregnancy", "Masochism",
	"Masturbation", "Mating Press", "Nakadashi", "Netorare", "Netorase", "Netori",
	"Omegaverse", "Oyakodon", "Pet Play", "Prostitution", "Public Sex", "Rape",
	"Rimjob", "Sadism", "Scat", "Scissoring", "Sex Toys", "Shimaidon",
	"Sixty-nine", "Squirting", "Sumata", "Swapping", "Sweat", "Tentacles",
	"Threesome", "Virginity", "Vore", "Voyeur", "Watersports", "Zoophilia",
}

// AdultTagNames returns the seed plus any stored adult names, fetching the
// vocabulary when empty; it never returns nil, so the guard fails closed.
func (s *JobService) AdultTagNames(ctx context.Context) []string {
	s.adultMu.Lock()
	if time.Now().Before(s.adultExp) && s.adultTags != nil {
		defer s.adultMu.Unlock()
		return s.adultTags
	}
	s.adultMu.Unlock()

	tags, err := s.want.catalog.ContentTags(ctx)
	if (err != nil || len(tags) == 0) && ctx.Err() == nil {
		if refreshErr := s.refreshTagVocabulary(ctx); refreshErr == nil {
			tags, err = s.want.catalog.ContentTags(ctx)
		}
	}

	names := make(map[string]bool, len(seedAdultTags)+len(tags))
	for _, n := range seedAdultTags {
		names[n] = true
	}
	if err == nil {
		for _, t := range tags {
			if t.IsAdult {
				names[t.Name] = true
			}
		}
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}

	s.adultMu.Lock()
	s.adultTags = out
	if err == nil && len(tags) > 0 {
		s.adultExp = time.Now().Add(10 * time.Minute)
	}
	s.adultMu.Unlock()
	return out
}

// ContentTagOptions returns the stored tag/genre vocabulary for pickers,
// fetching it once from AniList when nothing is stored yet.
func (s *JobService) ContentTagOptions(ctx context.Context) ([]catalog.ContentTag, error) {
	tags, err := s.want.catalog.ContentTags(ctx)
	if err != nil || len(tags) > 0 {
		return tags, err
	}
	if err := s.refreshTagVocabulary(ctx); err != nil {
		return nil, err
	}
	return s.want.catalog.ContentTags(ctx)
}

// encryptLegacyTokens re-encrypts plaintext AniList tokens left over from
// before at-rest encryption existed.
func (s *JobService) encryptLegacyTokens(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, access_token FROM user_anilist`)
	if err != nil {
		return err
	}
	type row struct {
		id    int64
		token string
	}
	var legacy []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.token) == nil && r.token != "" && !strings.HasPrefix(r.token, encPrefix) {
			legacy = append(legacy, r)
		}
	}
	rows.Close()
	for _, r := range legacy {
		enc, err := s.secrets.Encrypt(r.token)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE user_anilist SET access_token = ? WHERE user_id = ?`, enc, r.id); err != nil {
			return err
		}
	}
	return nil
}

// EnqueueAniListSync queues a progress sync for one user right now.
func (s *JobService) EnqueueAniListSync(ctx context.Context, userID int64) error {
	if _, _, ok := s.aniListIdentity(ctx, userID); !ok {
		return s.aniListError(ctx, userID)
	}
	_, err := s.enqueueExact(ctx, jobs.TypeSyncAniList, JobPayload{UserID: userID}, time.Now())
	return err
}

// AniListLibrary returns the connected user's AniList manga list.
func (s *JobService) AniListLibrary(ctx context.Context) ([]catalog.AniListEntry, error) {
	ctx, aid, ok := s.aniListIdentity(ctx, auth.UserID(ctx))
	if !ok {
		return nil, fmt.Errorf("no AniList account connected")
	}
	return s.want.AniList().UserList(ctx, aid)
}

// PushAniListEntry pushes the user's local progress and list status for a
// title to AniList in the background, creating the entry if it doesn't exist.
// Never downgrades remote progress or manual states. Failures are logged only.
func (s *JobService) PushAniListEntry(ctx context.Context, userID, titleID int64) {
	s.pushAniListEntry(ctx, userID, titleID, false)
}

// PushAniListEntryExact pushes local progress as authoritative, lowering the
// remote entry when the user has explicitly unread chapters, so a later sync
// does not pull the stale higher progress back in.
func (s *JobService) PushAniListEntryExact(ctx context.Context, userID, titleID int64) {
	s.pushAniListEntry(ctx, userID, titleID, true)
}

func (s *JobService) pushAniListEntry(ctx context.Context, userID, titleID int64, exact bool) {
	ctx = context.WithoutCancel(ctx)
	ctx, _, ok := s.aniListIdentity(ctx, userID)
	if !ok {
		return
	}
	mediaID, ok := s.aniListMediaID(ctx, titleID)
	if !ok {
		return
	}
	local, status, err := s.localAniListState(ctx, userID, titleID)
	if err != nil {
		return
	}
	go func() {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := s.reconcileAniListEntry(pctx, mediaID, local, status, exact); err != nil {
			log.Printf("anilist entry push (media %d): %v", mediaID, err)
			return
		}
		s.invalidateRecs(userID)
	}()
}

// reconcileAniListEntry raises the remote entry to at least the local
// progress/status, creating it when absent. When exact is set, progress is
// pushed even when it is lower than remote (an explicit unread). Manual list
// statuses are never downgraded.
func (s *JobService) reconcileAniListEntry(ctx context.Context, mediaID, local int, desired string, exact bool) error {
	remote, rstatus, found, err := s.want.AniList().MediaEntry(ctx, mediaID)
	if err != nil {
		return err
	}
	if !found {
		return s.want.AniList().SaveEntry(ctx, mediaID, local, desired)
	}
	progress := -1
	if (exact && local != remote) || local > remote {
		progress = local
	}
	status := ""
	if aniListStatusRank(desired) > aniListStatusRank(rstatus) {
		status = desired
	}
	if progress < 0 && status == "" {
		return nil
	}
	return s.want.AniList().SaveEntry(ctx, mediaID, progress, status)
}

// aniListMediaID resolves a tracked title to its AniList media id.
func (s *JobService) aniListMediaID(ctx context.Context, titleID int64) (int, bool) {
	title, err := s.lib.GetTitle(ctx, titleID)
	if err != nil || title.CatalogMangaID == nil {
		return 0, false
	}
	m, err := s.want.GetManga(ctx, *title.CatalogMangaID)
	if err != nil {
		return 0, false
	}
	mediaID, err := strconv.Atoi(m.ProviderID)
	if err != nil {
		return 0, false
	}
	return mediaID, true
}

// markAniListDropped sets a removed title to DROPPED on the acting user's
// AniList list, in the background and only on explicit request.
func (s *JobService) markAniListDropped(ctx context.Context, userID int64, mediaID int) {
	ctx = context.WithoutCancel(ctx)
	actx, _, ok := s.aniListIdentity(ctx, userID)
	if !ok {
		return
	}
	go func() {
		pctx, cancel := context.WithTimeout(actx, 3*time.Minute)
		defer cancel()
		if err := s.want.AniList().SaveEntry(pctx, mediaID, -1, "DROPPED"); err != nil {
			log.Printf("anilist drop (media %d): %v", mediaID, err)
			return
		}
		s.invalidateRecs(userID)
	}()
}

// deleteAniListEntry removes a title from the acting user's AniList list
// entirely, in the background.
func (s *JobService) deleteAniListEntry(ctx context.Context, userID int64, mediaID int) {
	ctx = context.WithoutCancel(ctx)
	actx, aid, ok := s.aniListIdentity(ctx, userID)
	if !ok {
		return
	}
	go func() {
		pctx, cancel := context.WithTimeout(actx, 3*time.Minute)
		defer cancel()
		if err := s.want.AniList().DeleteEntry(pctx, aid, mediaID); err != nil {
			log.Printf("anilist delete (media %d): %v", mediaID, err)
			return
		}
		s.invalidateRecs(userID)
	}()
}

// ToggleFavourite flips the acting user's favourite and mirrors it to their
// AniList account in the background.
func (s *JobService) ToggleFavourite(ctx context.Context, titleID int64) (bool, error) {
	fav, err := s.lib.ToggleFavourite(ctx, titleID)
	if err != nil {
		return false, err
	}
	s.pushAniListFavourite(ctx, auth.UserID(ctx), titleID, fav)
	return fav, nil
}

// pushAniListFavourite reconciles the remote favourite state to want. AniList
// only offers a toggle, so the current state is read first.
func (s *JobService) pushAniListFavourite(ctx context.Context, userID, titleID int64, want bool) {
	ctx = context.WithoutCancel(ctx)
	actx, _, ok := s.aniListIdentity(ctx, userID)
	if !ok {
		return
	}
	mediaID, ok := s.aniListMediaID(ctx, titleID)
	if !ok {
		return
	}
	go func() {
		pctx, cancel := context.WithTimeout(actx, 3*time.Minute)
		defer cancel()
		remote, err := s.want.AniList().IsFavourite(pctx, mediaID)
		if err != nil {
			log.Printf("anilist favourite read (media %d): %v", mediaID, err)
			return
		}
		if remote != want {
			if err := s.want.AniList().ToggleFavourite(pctx, mediaID); err != nil {
				log.Printf("anilist favourite toggle (media %d): %v", mediaID, err)
			}
		}
	}()
}

// localFavouriteIDs maps AniList provider ids of the user's favourites.
func (s *JobService) localFavouriteIDs(ctx context.Context, userID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cm.provider_id
		FROM user_favourites uf
		JOIN titles t ON t.id = uf.title_id
		JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE uf.user_id = ? AND cm.provider = ?`, userID, catalog.AniListProvider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var pid string
		if rows.Scan(&pid) == nil {
			out[pid] = true
		}
	}
	return out, rows.Err()
}

// SyncAniListTitle reconciles a single title with the acting user's AniList
// list right now: pulls remote progress down, then pushes progress/status up.
func (s *JobService) SyncAniListTitle(ctx context.Context, titleID int64) error {
	userID := auth.UserID(ctx)
	actx, _, ok := s.aniListIdentity(ctx, userID)
	if !ok {
		return fmt.Errorf("no AniList account connected")
	}
	mediaID, ok := s.aniListMediaID(ctx, titleID)
	if !ok {
		return fmt.Errorf("title has no AniList link")
	}
	remote, _, found, err := s.want.AniList().MediaEntry(actx, mediaID)
	if err != nil {
		return err
	}
	if found && remote > 0 {
		local, _, err := s.localAniListState(ctx, userID, titleID)
		if err != nil {
			return err
		}
		if remote > local {
			if _, err := s.lib.MarkChaptersReadThrough(ctx, titleID, remote); err != nil {
				return err
			}
		}
	}
	local, status, err := s.localAniListState(ctx, userID, titleID)
	if err != nil {
		return err
	}
	return s.reconcileAniListEntry(actx, mediaID, local, status, false)
}

// RelatedManga returns AniList relations/recommendations for a catalog entry.
func (s *JobService) RelatedManga(ctx context.Context, catalogID int64, limit int) ([]catalog.Manga, error) {
	// Unauthenticated: see SearchAniList.
	m, err := s.want.GetManga(ctx, catalogID)
	if err != nil {
		return nil, err
	}
	id, err := strconv.Atoi(m.ProviderID)
	if err != nil {
		return nil, fmt.Errorf("catalog entry %d has no AniList id", catalogID)
	}
	return s.want.AniList().Related(ctx, id, limit)
}

// TrendingManga returns currently trending AniList manga.
func (s *JobService) TrendingManga(ctx context.Context, limit int) ([]catalog.Manga, error) {
	return s.want.AniList().Trending(ctx, limit) // unauthenticated: see SearchAniList
}

// RecommendedManga returns AniList recommendations seeded from the acting
// user's list, falling back to global trending without a connected account.
func (s *JobService) RecommendedManga(ctx context.Context, limit int) ([]catalog.Manga, error) {
	uid := auth.UserID(ctx)
	items, ok := s.recsFromCache(uid)
	if !ok {
		var personalized bool
		var err error
		items, personalized, err = s.recommendedManga(ctx, limit)
		if err != nil {
			return nil, err
		}
		// Fallbacks stay uncached so a fresh connect shows up immediately.
		if personalized {
			s.cacheRecs(uid, items)
		}
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *JobService) recsFromCache(uid int64) ([]catalog.Manga, bool) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	c, ok := s.recCache[uid]
	if !ok || !time.Now().Before(c.expires) {
		return nil, false
	}
	return append([]catalog.Manga(nil), c.items...), true
}

func (s *JobService) cacheRecs(uid int64, items []catalog.Manga) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	if s.recCache == nil {
		s.recCache = map[int64]cachedRecs{}
	}
	s.recCache[uid] = cachedRecs{items: append([]catalog.Manga(nil), items...), expires: time.Now().Add(recommendationTTL)}
}

// invalidateRecs drops a user's cached recommendations on AniList changes.
func (s *JobService) invalidateRecs(userID int64) {
	s.recMu.Lock()
	delete(s.recCache, userID)
	s.recMu.Unlock()
}

func (s *JobService) recommendedManga(ctx context.Context, limit int) ([]catalog.Manga, bool, error) {
	actx, aid, ok := s.aniListIdentity(ctx, auth.UserID(ctx))
	if !ok {
		items, err := s.TrendingManga(ctx, limit)
		return items, false, err
	}
	entries, err := s.want.AniList().UserList(actx, aid)
	if err != nil || len(entries) == 0 {
		items, err := s.TrendingManga(ctx, limit)
		return items, false, err
	}
	onList := make(map[string]bool, len(entries))
	var seeds []catalog.AniListEntry
	for _, e := range entries {
		onList[e.Manga.ProviderID] = true
		if e.Status == "CURRENT" || e.Status == "COMPLETED" {
			seeds = append(seeds, e)
		}
	}
	if len(seeds) == 0 {
		seeds = entries
	}
	const maxSeeds = 4
	if len(seeds) > maxSeeds {
		// Spread seeds across the list so recommendations vary between titles
		// rather than always deriving from the same first entries.
		spread := make([]catalog.AniListEntry, 0, maxSeeds)
		for i := 0; i < maxSeeds; i++ {
			spread = append(spread, seeds[i*len(seeds)/maxSeeds])
		}
		seeds = spread
	}
	var out []catalog.Manga
	seen := map[string]bool{}
	for _, seed := range seeds {
		mediaID, err := strconv.Atoi(seed.Manga.ProviderID)
		if err != nil {
			continue
		}
		items, err := s.want.AniList().Related(ctx, mediaID, limit) // unauthenticated: see SearchAniList
		if err != nil {
			continue // one bad seed shouldn't empty the grid
		}
		for _, m := range items {
			if onList[m.ProviderID] || seen[m.ProviderID] {
				continue
			}
			seen[m.ProviderID] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		items, err := s.TrendingManga(ctx, limit)
		return items, false, err
	}
	return out, true, nil
}

// MangaByIDs returns catalog manga keyed by ID in one query.
func (s *JobService) MangaByIDs(ctx context.Context, ids []int64) (map[int64]catalog.Manga, error) {
	return s.want.catalog.MangaByIDs(ctx, ids)
}

// CollectionEdges returns the stored relation graph as (from, to) provider-id pairs.
func (s *JobService) CollectionEdges(ctx context.Context) ([][2]string, error) {
	return s.want.catalog.CollectionEdges(ctx)
}

// CustomCollections lists the acting user's manually curated collections.
func (s *JobService) CustomCollections(ctx context.Context) ([]library.Collection, error) {
	return s.want.library.Collections(ctx)
}

// CollectionMembers maps custom collection id -> member title ids.
func (s *JobService) CollectionMembers(ctx context.Context) (map[int64][]int64, error) {
	return s.want.library.CollectionMembers(ctx)
}

// SmartPins maps smart-collection key -> pinned title ids.
func (s *JobService) SmartPins(ctx context.Context) (map[string][]int64, error) {
	return s.want.library.SmartPins(ctx)
}

func (s *JobService) CreateCollection(ctx context.Context, name string) (int64, error) {
	return s.want.library.CreateCollection(ctx, name)
}

func (s *JobService) RenameCollection(ctx context.Context, id int64, name string) error {
	return s.want.library.RenameCollection(ctx, id, name)
}

func (s *JobService) DeleteCollection(ctx context.Context, id int64) error {
	return s.want.library.DeleteCollection(ctx, id)
}

func (s *JobService) AddToCollection(ctx context.Context, collectionID, titleID int64) error {
	return s.want.library.AddCollectionMember(ctx, collectionID, titleID)
}

func (s *JobService) RemoveFromCollection(ctx context.Context, collectionID, titleID int64) error {
	return s.want.library.RemoveCollectionMember(ctx, collectionID, titleID)
}

func (s *JobService) PinToSmart(ctx context.Context, smartKey string, titleID int64) error {
	return s.want.library.AddSmartPin(ctx, smartKey, titleID)
}

func (s *JobService) RemoveSmartPin(ctx context.Context, smartKey string, titleID int64) error {
	return s.want.library.RemoveSmartPin(ctx, smartKey, titleID)
}

func (s *JobService) GetManga(ctx context.Context, catalogID int64) (catalog.Manga, error) {
	return s.want.GetManga(ctx, catalogID)
}

// CatalogMangaByAniList returns catalog metadata for an AniList id, fetching and
// caching it from AniList when it is not yet in the local catalog. Lets users
// view a manga's detail page before it is added to the library.
func (s *JobService) CatalogMangaByAniList(ctx context.Context, anilistID int) (catalog.Manga, error) {
	pid := strconv.Itoa(anilistID)
	if m, ok, err := s.want.catalog.MangaByProvider(ctx, catalog.AniListProvider, pid); err != nil {
		return catalog.Manga{}, err
	} else if ok {
		return m, nil
	}
	m, err := s.want.AniList().Get(ctx, anilistID)
	if err != nil {
		return catalog.Manga{}, err
	}
	return s.want.catalog.UpsertManga(ctx, m)
}

// RemoveTitleFiles removes a title and, when deleteFiles is set, its
// downloaded folder on disk. Without deletion the folder reappears on the
// Import page as an untracked candidate.
func (s *JobService) RemoveTitleFiles(ctx context.Context, id int64, deleteFiles, deleteAniList bool) (library.Title, error) {
	title, err := s.lib.GetTitle(ctx, id)
	if err != nil {
		return library.Title{}, err
	}
	var filesDir string
	if deleteFiles {
		cfg, _, err := s.RuntimeConfig(ctx)
		if err != nil {
			return library.Title{}, err
		}
		// Resolve (and validate) before the rows disappear.
		if filesDir, err = s.lib.TitleFilesDir(cfg, title); err != nil {
			return library.Title{}, err
		}
	}
	// Resolve the AniList link before the rows disappear.
	mediaID, hasAniList := s.aniListMediaID(ctx, id)
	if err := s.cancelTitleJobs(ctx, id); err != nil {
		return library.Title{}, err
	}
	if _, err := s.lib.RemoveTitle(ctx, id); err != nil {
		return library.Title{}, err
	}
	if hasAniList {
		if deleteAniList {
			s.deleteAniListEntry(ctx, auth.UserID(ctx), mediaID)
		} else {
			s.markAniListDropped(ctx, auth.UserID(ctx), mediaID)
		}
	}
	if filesDir != "" {
		if _, statErr := os.Stat(filesDir); os.IsNotExist(statErr) {
			return title, fmt.Errorf("title removed, but no files found at %s — check the download directory setting", filesDir)
		}
		if err := os.RemoveAll(filesDir); err != nil {
			return title, fmt.Errorf("title removed, but deleting %s failed: %w", filesDir, err)
		}
	}
	return title, nil
}

// SetMonitored toggles monitoring for a tracked title.
// SetRefreshInterval sets a title's custom refresh cadence (empty = global).
func (s *JobService) SetRefreshInterval(ctx context.Context, id int64, interval string) error {
	return s.lib.SetRefreshInterval(ctx, id, interval)
}

// SetLanguageMode stores the language decision; choosing "all" refreshes so
// the newly eligible chapters get discovered.
func (s *JobService) SetLanguageMode(ctx context.Context, id int64, mode string) error {
	if err := s.lib.SetLanguageMode(ctx, id, mode); err != nil {
		return err
	}
	if mode == "all" {
		if _, err := s.Enqueue(ctx, jobs.TypeRefreshTitle, id, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

func (s *JobService) SetMonitored(ctx context.Context, id int64, monitored bool) error {
	return s.lib.SetMonitored(ctx, id, monitored)
}

// SearchAniList searches AniList and stores returned metadata locally.
func (s *JobService) SearchAniList(ctx context.Context, query string, limit int, filter catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	// Unauthenticated: a token makes AniList pre-filter adult entries by the
	// account's own 18+ setting, bypassing kaodoku's per-user content guard.
	return s.want.SearchAniList(ctx, query, limit, filter)
}

// AddAniListWanted adds an AniList title to wanted; a non-nil allowed guard is
// checked before anything is persisted.
func (s *JobService) AddAniListWanted(ctx context.Context, anilistID int, allowed func(catalog.Manga) bool) (catalog.Manga, error) {
	return s.want.AddAniListWanted(ctx, anilistID, allowed)
}

// GetMatch returns one source match.
func (s *JobService) GetMatch(ctx context.Context, id int64) (catalog.Match, error) {
	return s.want.catalog.GetMatch(ctx, id)
}

// ListWanted returns wanted canonical titles.
func (s *JobService) ListWanted(ctx context.Context) ([]catalog.Manga, error) {
	return s.want.ListWanted(ctx)
}

// ExploreDownloads lists untracked folders in the download directory.
func (s *JobService) ExploreDownloads(ctx context.Context) ([]ImportCandidate, error) {
	cfg, _, err := s.RuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s.want.ExploreDownloads(ctx, cfg.DownloadDir)
}

// ImportFolder tracks an existing download folder against an AniList entry.
func (s *JobService) ImportFolder(ctx context.Context, folder string, anilistID int) (library.Title, error) {
	cfg, _, err := s.RuntimeConfig(ctx)
	if err != nil {
		return library.Title{}, err
	}
	title, err := s.want.ImportFolder(ctx, cfg.DownloadDir, folder, anilistID)
	if err != nil {
		return library.Title{}, err
	}
	s.PushAniListEntry(ctx, auth.UserID(ctx), title.ID)
	if err := s.enqueueSourceSearchForTitle(ctx, title); err != nil {
		return title, err
	}
	return title, nil
}

func (s *JobService) Volumes(ctx context.Context, titleID int64) ([]library.Volume, error) {
	return s.lib.Volumes(ctx, titleID)
}

func (s *JobService) GetVolume(ctx context.Context, id int64) (library.Volume, error) {
	return s.lib.GetVolume(ctx, id)
}

// MarkPageReadAt marks a page read at an explicit time (offline replay).
func (s *JobService) MarkPageReadAt(ctx context.Context, chapterID int64, page, totalPages int, readAt string) (library.ChapterReadStatus, error) {
	return s.lib.MarkPageReadAt(ctx, chapterID, page, totalPages, readAt)
}

// ProgressSince returns chapter and volume progress touched after since.
func (s *JobService) ProgressSince(ctx context.Context, since string) ([]library.ChapterReadStatus, []library.Volume, error) {
	chs, err := s.lib.ChaptersReadSince(ctx, since)
	if err != nil {
		return nil, nil, err
	}
	vols, err := s.lib.VolumesReadSince(ctx, since)
	return chs, vols, err
}

// ReadProgressIDs returns all chapter/volume ids with progress rows.
func (s *JobService) ReadProgressIDs(ctx context.Context) ([]int64, []int64, error) {
	return s.lib.ReadProgressIDs(ctx)
}

func (s *JobService) SetVolumeRead(ctx context.Context, id int64, read bool) error {
	return s.lib.SetVolumeRead(ctx, id, read)
}

func (s *JobService) Screens(ctx context.Context) ([]library.Screen, error) {
	return s.lib.Screens(ctx)
}

func (s *JobService) GetScreen(ctx context.Context, id int64) (library.Screen, error) {
	return s.lib.GetScreen(ctx, id)
}

func (s *JobService) SaveScreen(ctx context.Context, sc library.Screen) (int64, error) {
	return s.lib.SaveScreen(ctx, sc)
}

func (s *JobService) ReorderScreens(ctx context.Context, ids []int64) error {
	return s.lib.ReorderScreens(ctx, ids)
}

func (s *JobService) DeleteScreen(ctx context.Context, id int64) error {
	return s.lib.DeleteScreen(ctx, id)
}

func (s *JobService) LastReadAt(ctx context.Context) (map[int64]time.Time, error) {
	return s.lib.LastReadAt(ctx)
}

func (s *JobService) LatestArrivals(ctx context.Context) (map[int64]library.Arrival, error) {
	return s.lib.LatestArrivals(ctx)
}

func (s *JobService) MarkVolumePageRead(ctx context.Context, volumeID int64, page, totalPages int) (library.Volume, error) {
	return s.lib.MarkVolumePageRead(ctx, volumeID, page, totalPages)
}

func (s *JobService) VolumesReaderProgress(ctx context.Context, titleID int64) (library.TitleReadProgress, error) {
	return s.lib.VolumesReaderProgress(ctx, titleID)
}

func (s *JobService) SetVolumeRangeRead(ctx context.Context, titleID int64, from, to float64, read bool) (int, error) {
	return s.lib.SetVolumeRangeRead(ctx, titleID, from, to, read)
}

func (s *JobService) VolumeThumb(ctx context.Context, id int64) ([]byte, string, error) {
	return s.lib.VolumeThumb(ctx, id)
}

func (s *JobService) VolumeCover(ctx context.Context, id int64) ([]byte, string, error) {
	return s.lib.VolumeCover(ctx, id)
}

func (s *JobService) TitleCover(ctx context.Context, id int64) ([]byte, string, error) {
	return s.lib.TitleCover(ctx, id)
}

func (s *JobService) SetTitleCover(ctx context.Context, id int64, blob []byte, mime string) error {
	return s.lib.SetTitleCover(ctx, id, blob, mime)
}

func (s *JobService) SetVolumeCover(ctx context.Context, id int64, blob []byte, mime string) error {
	return s.lib.SetVolumeCover(ctx, id, blob, mime)
}

// ImportVolumesFolder tracks a folder of volume files as a new title.
// Volumes are disk-only: no source search is enqueued.
func (s *JobService) ImportVolumesFolder(ctx context.Context, folder string, anilistID int) (library.Title, error) {
	cfg, _, err := s.RuntimeConfig(ctx)
	if err != nil {
		return library.Title{}, err
	}
	title, err := s.want.ImportVolumesFolder(ctx, cfg.DownloadDir, folder, anilistID)
	if err != nil {
		return library.Title{}, err
	}
	s.PushAniListEntry(ctx, auth.UserID(ctx), title.ID)
	return title, nil
}

// AttachVolumesFolder queues a background move of an untracked folder's
// volume files into an existing title (Chapters/Volumes split).
func (s *JobService) AttachVolumesFolder(ctx context.Context, folder string, titleID int64) (library.Title, error) {
	title, err := s.lib.GetTitle(ctx, titleID)
	if err != nil {
		return library.Title{}, err
	}
	if _, err := s.enqueueExact(ctx, jobs.TypeAttachVolumes, JobPayload{TitleID: titleID, Folder: folder}, time.Now()); err != nil {
		return library.Title{}, err
	}
	return title, nil
}

func (s *JobService) sourceAllowed(ctx context.Context, sourceID string) error {
	u := auth.FromContext(ctx)
	if u == nil || u.AllowAdult {
		return nil
	}
	if src, err := s.src.GetSource(ctx, sourceID); err == nil && src.NSFW {
		return fmt.Errorf("that source is not available for this account")
	}
	return nil
}

// LinkTitleSource links a tracked title to a matched source.
func (s *JobService) LinkTitleSource(ctx context.Context, titleID, matchID int64) (library.Title, error) {
	match, err := s.GetMatch(ctx, matchID)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.sourceAllowed(ctx, match.SourceID); err != nil {
		return library.Title{}, err
	}
	title, err := s.want.LinkTitleSource(ctx, titleID, matchID)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.enqueueRefreshForTitle(ctx, title); err != nil {
		return title, err
	}
	return title, nil
}

// ClearMatches drops cached source decisions for a manga.
func (s *JobService) ClearMatches(ctx context.Context, catalogID int64) error {
	return s.want.catalog.DeleteMatches(ctx, catalogID)
}

// MatchSources finds source matches for one canonical title.
func (s *JobService) MatchSources(ctx context.Context, catalogID int64) ([]catalog.Match, error) {
	cfg, logSvc, err := s.RuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s.want.MatchSources(ctx, cfg, logSvc, catalogID)
}

// ListMatches returns persisted source matches.
func (s *JobService) ListMatches(ctx context.Context, catalogID int64) ([]catalog.Match, error) {
	return s.want.ListMatches(ctx, catalogID)
}

// TrackMatch adds a selected match to the tracked library.
func (s *JobService) TrackMatch(ctx context.Context, matchID int64, output string, monitored bool, refreshInterval string) (library.Title, error) {
	title, err := s.want.TrackMatch(ctx, matchID, output, monitored, refreshInterval)
	if err != nil {
		return library.Title{}, err
	}
	s.PushAniListEntry(ctx, auth.UserID(ctx), title.ID)
	if err := s.enqueueRefreshForTitle(ctx, title); err != nil {
		return title, err
	}
	return title, nil
}

// TrackMatchDefault adds a selected match using catalog status for monitoring.
func (s *JobService) TrackMatchDefault(ctx context.Context, matchID int64, output string, refreshInterval string) (library.Title, error) {
	match, err := s.want.catalog.GetMatch(ctx, matchID)
	if err != nil {
		return library.Title{}, err
	}
	manga, err := s.want.catalog.GetManga(ctx, match.CatalogMangaID)
	if err != nil {
		return library.Title{}, err
	}
	return s.TrackMatch(ctx, matchID, output, defaultMonitored(manga), refreshInterval)
}

// SyncSources stores bundled profiles and, when set, a remote registry.
func (s *JobService) SyncSources(ctx context.Context, registryURL string) error {
	if err := s.src.SyncBuiltIn(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(registryURL) == "" {
		return nil
	}
	return s.src.SyncRegistry(ctx, registryURL)
}

// ListSources returns known source profiles.
func (s *JobService) ListSources(ctx context.Context) ([]sources.Source, error) {
	return s.src.ListSources(ctx)
}

func (s *JobService) SourceTitleCounts(ctx context.Context) (map[string]int, error) {
	return s.src.repo.TitleCounts(ctx)
}

// SourceFetchFailureCounts counts titles per source whose last chapter
// refresh failed.
func (s *JobService) SourceFetchFailureCounts(ctx context.Context) (map[string]int, error) {
	return s.src.repo.FetchFailureCounts(ctx)
}

// GetSource returns one source profile.
func (s *JobService) GetSource(ctx context.Context, id string) (sources.Source, error) {
	return s.src.GetSource(ctx, id)
}

// ImportLocalSource stores a local DB-backed source profile.
func (s *JobService) ImportLocalSource(ctx context.Context, profile sources.Profile) error {
	if err := s.src.ImportLocal(ctx, profile); err != nil {
		return err
	}
	return s.enqueueRefreshForSource(ctx, profile.ID)
}

// SetSourceEnabled toggles a source on or off.
func (s *JobService) SetSourceEnabled(ctx context.Context, sourceID string, enabled bool) error {
	return s.src.SetEnabled(ctx, sourceID, enabled)
}

// RemoveLocalSource removes a local DB-backed source profile.
func (s *JobService) RemoveLocalSource(ctx context.Context, sourceID string) error {
	return s.src.RemoveLocal(ctx, sourceID)
}

// ExportSource returns one source profile as YAML.
func (s *JobService) ExportSource(ctx context.Context, sourceID string) ([]byte, error) {
	return s.src.ExportSource(ctx, sourceID)
}

// VerifySource checks one source profile.
func (s *JobService) VerifySource(ctx context.Context, cfg *config.Config, logSvc ui.Log, sourceID string) (SourceVerifyResult, error) {
	result, err := s.src.VerifySource(ctx, cfg, logSvc, sourceID)
	if err == nil {
		if cerr := s.want.catalog.ClearMissesForSource(ctx, result.SourceID); cerr != nil {
			logSvc.Warnf("Clearing match misses for %s failed: %v\n", result.SourceID, cerr)
		}
	}
	return result, err
}

// VerifySourceURL probes a user-specified manga URL within a chosen source,
// reporting how many chapters it finds so the user can confirm before linking.
func (s *JobService) VerifySourceURL(ctx context.Context, sourceID, rawURL string) (SourceTestResult, error) {
	src, err := s.sourceForURL(ctx, sourceID, rawURL)
	if err != nil {
		return SourceTestResult{}, err
	}
	cfg, logSvc, err := s.RuntimeConfig(ctx)
	if err != nil {
		return SourceTestResult{}, err
	}
	profile := src.Profile
	profile.SampleMangaURL = strings.TrimSpace(rawURL)
	useSolver := src.RequiresBrowserSolver || src.ChapterFetch == sources.FetchSolver
	useBrowser := src.RequiresBrowserDownload || src.ImageFetch == sources.FetchBrowser
	return s.src.TestProfile(ctx, cfg, logSvc, profile, useSolver, useBrowser), nil
}

// LinkTitleSourceURL links a title to a user-specified URL served by a chosen
// source, so its scraper and fetch methods apply.
func (s *JobService) LinkTitleSourceURL(ctx context.Context, titleID int64, sourceID, rawURL string) (library.Title, error) {
	src, err := s.sourceForURL(ctx, sourceID, rawURL)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.sourceAllowed(ctx, src.ID); err != nil {
		return library.Title{}, err
	}
	title, err := s.want.LinkTitleURL(ctx, titleID, strings.TrimSpace(rawURL), src.ID)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.enqueueRefreshForTitle(ctx, title); err != nil {
		return title, err
	}
	return title, nil
}

// sourceForURL validates that rawURL is a well-formed page on the chosen source.
func (s *JobService) sourceForURL(ctx context.Context, sourceID, rawURL string) (sources.Source, error) {
	rawURL = strings.TrimSpace(rawURL)
	if u, err := url.ParseRequestURI(rawURL); err != nil || !strings.HasPrefix(u.Scheme, "http") {
		return sources.Source{}, fmt.Errorf("enter a full http(s) URL")
	}
	src, err := s.src.GetSource(ctx, sourceID)
	if err != nil {
		return sources.Source{}, err
	}
	if _, ok := MatchSourceForURL([]sources.Source{src}, rawURL); !ok {
		return sources.Source{}, fmt.Errorf("that URL is not on %s", src.Name)
	}
	return src, nil
}

// LinkTitleToSource links a title to a registered source by ID, using that
// source's manga URL. Its fetch methods and image extensions then apply.
func (s *JobService) LinkTitleToSource(ctx context.Context, titleID int64, sourceID string) (library.Title, error) {
	src, err := s.src.GetSource(ctx, sourceID)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.sourceAllowed(ctx, src.ID); err != nil {
		return library.Title{}, err
	}
	if _, err := url.ParseRequestURI(src.SampleMangaURL); err != nil {
		return library.Title{}, fmt.Errorf("source %q has no manga URL to link", src.Name)
	}
	title, err := s.want.LinkTitleURL(ctx, titleID, src.SampleMangaURL, src.ID)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.enqueueRefreshForTitle(ctx, title); err != nil {
		return title, err
	}
	return title, nil
}

// ServiceHealth is the reachability of one external helper service.
type ServiceHealth struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Enabled  bool   `json:"enabled"`
	Healthy  bool   `json:"healthy"`
	Version  string `json:"version,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ServicesHealth reports reachability of each configured helper service.
func (s *JobService) ServicesHealth(ctx context.Context) []ServiceHealth {
	cfg, _, err := s.RuntimeConfig(ctx)
	if err != nil || cfg == nil {
		return nil
	}
	check := func(name, endpoint string, enabled bool, probe func(context.Context) (string, error)) ServiceHealth {
		h := ServiceHealth{Name: name, Endpoint: endpoint, Enabled: enabled}
		if !enabled {
			return h
		}
		pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		version, err := probe(pctx)
		if err != nil {
			h.Error = err.Error()
			return h
		}
		h.Healthy, h.Version = true, version
		return h
	}
	probes := []func() ServiceHealth{
		func() ServiceHealth {
			return check("FlareSolverr (page solver)", cfg.BrowserSolver.Endpoint, cfg.BrowserSolver.Enabled, func(c context.Context) (string, error) {
				return "", browserfetch.NewFlareSolverr(cfg.BrowserSolver.Endpoint, time.Duration(cfg.BrowserSolver.TimeoutSeconds)*time.Second, nil).Health(c)
			})
		},
		func() ServiceHealth {
			return check("Browser downloader (Selenium)", cfg.BrowserDownload.Endpoint, cfg.BrowserDownload.Enabled, func(c context.Context) (string, error) {
				return "", browserdownload.New(cfg.BrowserDownload.Endpoint, 8*time.Second, nil).Health(c)
			})
		},
		func() ServiceHealth {
			return check("Kotatsu worker (sources)", cfg.KotatsuWorker.Endpoint, cfg.KotatsuWorker.Enabled, func(c context.Context) (string, error) {
				h, err := kotatsu.New(cfg.KotatsuWorker.Endpoint, cfg.KotatsuWorker.APIKey).Health(c)
				if h.Solver {
					return h.Version + " · solver", err
				}
				return h.Version, err
			})
		},
	}
	out := make([]ServiceHealth, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = p()
		}()
	}
	wg.Wait()
	return out
}

// SetSourceMethods overrides a source's chapter/image fetch methods.
func (s *JobService) SetSourceMethods(ctx context.Context, sourceID, chapterFetch, imageFetch string) error {
	if err := s.src.SetFetchMethods(ctx, sourceID, chapterFetch, imageFetch); err != nil {
		return err
	}
	return s.enqueueRefreshForSource(ctx, sourceID)
}

func (s *JobService) KotatsuClient(ctx context.Context) *kotatsu.Client {
	cfg, _, err := s.RuntimeConfig(ctx)
	if err != nil || !cfg.KotatsuWorker.Enabled {
		return nil
	}
	return kotatsu.New(cfg.KotatsuWorker.Endpoint, cfg.KotatsuWorker.APIKey)
}

const kotatsuSourcesTTL = 5 * time.Minute

func (s *JobService) KotatsuSources(ctx context.Context) ([]kotatsu.Source, error) {
	kc := s.KotatsuClient(ctx)
	if kc == nil {
		return nil, fmt.Errorf("kotatsu worker is not enabled")
	}
	s.kotMu.Lock()
	defer s.kotMu.Unlock()
	if time.Now().Before(s.kotExp) {
		return s.kotSources, nil
	}
	list, err := kc.Sources(ctx)
	if err != nil {
		return nil, err
	}
	s.kotSources, s.kotExp = list, time.Now().Add(kotatsuSourcesTTL)

	go s.pushKotatsuConfigs(kc)
	return list, nil
}

// pushKotatsuConfigs restores user domain overrides into the worker's
// in-memory config.
func (s *JobService) pushKotatsuConfigs(kc *kotatsu.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srcs, err := s.src.ListSources(ctx)
	if err != nil {
		return
	}
	for _, src := range srcs {
		id, ok := strings.CutPrefix(src.Scraper, kotatsu.ScraperPrefix)
		if !ok || len(src.Domains) == 0 {
			continue
		}
		if err := kc.SetConfig(ctx, id, map[string]string{"domain": src.Domains[0]}); err != nil {
			log.Printf("push kotatsu domain for %s: %v", src.ID, err)
		}
	}
}

func (s *JobService) KotatsuSample(ctx context.Context, src kotatsu.Source) string {
	kc := s.KotatsuClient(ctx)
	if kc == nil {
		return src.BaseURL
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if res, err := kc.Search(sctx, src.ID, ""); err == nil && len(res) > 0 {
		return res[0].URL
	}
	return src.BaseURL
}

func (s *JobService) ImportKotatsuSource(ctx context.Context, src kotatsu.Source, version, sample string) error {
	id := kotatsu.ScraperPrefix + src.ID
	var langs []string
	if src.Language != "" {
		langs = []string{src.Language}
	}
	if strings.TrimSpace(sample) == "" {
		sample = src.BaseURL
	}
	if err := s.src.ImportLocal(ctx, sources.Profile{
		ID: id, Name: src.Name, BaseURL: src.BaseURL,
		SampleMangaURL: sample,
		Scraper:        id, NSFW: src.NSFW, Languages: langs, Enabled: true, Version: version,
	}); err != nil {
		return err
	}
	if err := s.src.SetEnabled(ctx, strings.ToLower(id), true); err != nil {
		return err
	}
	_, err := s.EnqueueSource(ctx, strings.ToLower(id), time.Now())
	return err
}

func (s *JobService) enqueueSourceSearchForTitle(ctx context.Context, title library.Title) error {
	if title.CatalogMangaID == nil {
		return nil
	}
	if _, err := s.EnqueueCatalog(ctx, jobs.TypeMatchSources, *title.CatalogMangaID, time.Now()); err != nil {
		return fmt.Errorf("queue source search: %w", err)
	}
	return nil
}

func (s *JobService) enqueueRefreshForTitle(ctx context.Context, title library.Title) error {
	if !strings.HasPrefix(strings.TrimSpace(title.SourceURL), "http") {
		return nil
	}
	payload := JobPayload{TitleID: title.ID, DownloadAfterRefresh: true}
	if _, err := s.enqueue(ctx, jobs.TypeRefreshTitle, payload, time.Now()); err != nil {
		return fmt.Errorf("queue chapter refresh: %w", err)
	}
	return nil
}

func (s *JobService) enqueueRefreshForSource(ctx context.Context, sourceID string) error {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil
	}
	titles, err := s.lib.ListTitles(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, title := range titles {
		if title.SourceID != sourceID || !title.Monitored {
			continue
		}
		errs = append(errs, s.enqueueRefreshForTitle(ctx, title))
	}
	return errors.Join(errs...)
}

// TestSource probes a candidate source profile with the chosen fetch methods.
func (s *JobService) TestSource(ctx context.Context, profile sources.Profile, useSolver, useBrowser bool) (SourceTestResult, error) {
	cfg, logSvc, err := s.RuntimeConfig(ctx)
	if err != nil {
		return SourceTestResult{}, err
	}
	return s.src.TestProfile(ctx, cfg, logSvc, profile, useSolver, useBrowser), nil
}

// Enqueue creates a job.
func (s *JobService) Enqueue(ctx context.Context, typ string, titleID int64, runAfter time.Time) (jobs.Job, error) {
	payload := JobPayload{TitleID: titleID}
	if typ == jobs.TypeDownloadMissing && titleID > 0 {
		payload.ResetFailed = true
	}
	return s.enqueue(ctx, typ, payload, runAfter)
}

// EnqueueSource creates a source-scoped job.
func (s *JobService) EnqueueSource(ctx context.Context, sourceID string, runAfter time.Time) (jobs.Job, error) {
	return s.enqueue(ctx, jobs.TypeVerifySource, JobPayload{SourceID: strings.TrimSpace(sourceID)}, runAfter)
}

func (s *JobService) EnqueueSourceVerification(ctx context.Context, runAfter time.Time) (int, error) {
	srcs, err := s.ListSources(ctx)
	if err != nil {
		return 0, err
	}
	counts := map[string]int{}
	var errs []error
	n := 0
	for _, src := range srcs {
		counts[src.Status]++
		if !src.Enabled {
			continue
		}
		_, err := s.EnqueueSource(ctx, src.ID, runAfter)
		if err == nil {
			n++
		}
		errs = append(errs, err)
	}
	log.Printf("source verification sweep: %d queued; current health: healthy %d, degraded %d, requires_cf %d, broken %d, unknown %d",
		n, counts[sources.StatusHealthy], counts[sources.StatusDegraded], counts[sources.StatusRequiresCF], counts[sources.StatusBroken], counts["unknown"])
	return n, errors.Join(errs...)
}

// EnqueueCatalog creates a catalog-scoped job.
func (s *JobService) EnqueueCatalog(ctx context.Context, typ string, catalogID int64, runAfter time.Time) (jobs.Job, error) {
	return s.enqueue(ctx, typ, JobPayload{CatalogID: catalogID}, runAfter)
}

func (s *JobService) enqueue(ctx context.Context, typ string, payload JobPayload, runAfter time.Time) (jobs.Job, error) {
	return s.enqueueJob(ctx, typ, payload, runAfter, true)
}

func (s *JobService) enqueueExact(ctx context.Context, typ string, payload JobPayload, runAfter time.Time) (jobs.Job, error) {
	return s.enqueueJob(ctx, typ, payload, runAfter, false)
}

// enqueueChild enqueues a per-title job spawned by a global job.
func (s *JobService) enqueueChild(ctx context.Context, typ string, payload JobPayload, runAfter time.Time, parentID int64) (jobs.Job, error) {
	if err := validateJob(typ, payload); err != nil {
		return jobs.Job{}, err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return jobs.Job{}, fmt.Errorf("encode job payload: %w", err)
	}
	return s.jobs.EnqueueChild(ctx, typ, string(data), runAfter, parentID)
}

func (s *JobService) enqueueJob(ctx context.Context, typ string, payload JobPayload, runAfter time.Time, useCover bool) (jobs.Job, error) {
	if err := validateJob(typ, payload); err != nil {
		return jobs.Job{}, err
	}
	if useCover {
		if job, ok, err := s.coveringJob(ctx, typ, payload); err != nil {
			return jobs.Job{}, err
		} else if ok {
			return job, nil
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return jobs.Job{}, fmt.Errorf("encode job payload: %w", err)
	}
	return s.jobs.Enqueue(ctx, typ, string(data), runAfter)
}

func (s *JobService) coveringJob(ctx context.Context, typ string, payload JobPayload) (jobs.Job, bool, error) {
	if payload.TitleID <= 0 || !titleScopedJob(typ) {
		return jobs.Job{}, false, nil
	}
	all, err := s.jobs.ActiveForTitle(ctx, typ, payload.TitleID)
	if err != nil {
		return jobs.Job{}, false, err
	}
	for _, job := range all {
		var existing JobPayload
		if json.Unmarshal([]byte(job.Payload), &existing) != nil {
			continue
		}
		if typ == jobs.TypeRefreshTitle && payload.DownloadAfterRefresh && !existing.DownloadAfterRefresh {
			continue
		}
		// A reset request is stronger than a plain download job: it must not
		// be swallowed by an active sweep job, or attempt-capped titles become
		// unrecoverable from the UI.
		if typ == jobs.TypeDownloadMissing && payload.ResetFailed && !existing.ResetFailed {
			continue
		}
		if existing.TitleID == 0 || existing.TitleID == payload.TitleID {
			return job, true, nil
		}
	}
	return jobs.Job{}, false, nil
}

// GetJob returns one job by id.
func (s *JobService) GetJob(ctx context.Context, id int64) (jobs.Job, error) {
	return s.jobs.Get(ctx, id)
}

func (s *JobService) RunningJobs() int {
	n := 0
	s.running.Range(func(_, _ any) bool { n++; return true })
	return n
}

// CancelJob aborts a job: a running job's context is cancelled; a queued or
// awaiting-retry job is marked cancelled so it never runs.
func (s *JobService) CancelJob(ctx context.Context, id int64) error {
	if v, ok := s.running.Load(id); ok {
		v.(context.CancelCauseFunc)(errJobCancelled)
	} else if _, err := s.jobs.Cancel(ctx, id); err != nil {
		return err
	}
	// Cancelling a global job also cancels everything it spawned.
	runningChildren, err := s.jobs.CancelChildren(ctx, id)
	if err != nil {
		return err
	}
	for _, child := range runningChildren {
		if v, ok := s.running.Load(child); ok {
			v.(context.CancelCauseFunc)(errJobCancelled)
		}
	}
	return nil
}

func (s *JobService) cancelTitleJobs(ctx context.Context, titleID int64) error {
	js, err := s.jobs.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, job := range js {
		if !titleScopedJob(job.Type) || !activeJobStatus(job.Status) {
			continue
		}
		var payload JobPayload
		if json.Unmarshal([]byte(job.Payload), &payload) != nil || payload.TitleID != titleID {
			continue
		}
		errs = append(errs, s.CancelJob(ctx, job.ID))
	}
	return errors.Join(errs...)
}

// List returns recent jobs.
func (s *JobService) List(ctx context.Context) ([]jobs.Job, error) {
	return s.jobs.List(ctx)
}

// RunDue drains due jobs; a call that overlaps a running drain hands its work to it and returns.
func (s *JobService) RunDue(ctx context.Context, cfg *config.Config, logSvc ui.Log) (RunSummary, error) {
	var total RunSummary
	s.rerun.Store(true)
	for s.rerun.Load() && ctx.Err() == nil {
		if !s.runMu.TryLock() {
			return total, nil
		}
		for s.rerun.Swap(false) {
			summary, err := s.drainQueue(ctx, cfg, logSvc)
			total.Done += summary.Done
			total.Failed += summary.Failed
			if err != nil {
				s.runMu.Unlock()
				return total, err
			}
		}
		s.runMu.Unlock()
	}
	return total, nil
}

func (s *JobService) drainQueue(ctx context.Context, cfg *config.Config, logSvc ui.Log) (RunSummary, error) {
	var summary RunSummary
	var summaryMu sync.Mutex
	var firstErr error
	var errMu sync.Mutex
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Outcomes must be persisted even when ctx is cancelled mid-job;
	// marking with the cancelled ctx would strand the job as running.
	markCtx := context.WithoutCancel(ctx)

	workers := s.jobWorkers
	if workers < 1 {
		workers = 1
	}
	setErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}
	add := func(done, failed int) {
		summaryMu.Lock()
		summary.Done += done
		summary.Failed += failed
		summaryMu.Unlock()
	}

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for runCtx.Err() == nil {
				job, ok, err := s.jobs.ClaimNext(runCtx)
				if err != nil {
					setErr(err)
					return
				}
				if !ok {
					return
				}
				done, failed, err := s.runClaimedJob(runCtx, markCtx, cfg, logSvc, job)
				add(done, failed)
				if err != nil {
					setErr(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return summary, firstErr
}

func (s *JobService) runClaimedJob(ctx, markCtx context.Context, cfg *config.Config, logSvc ui.Log, job jobs.Job) (done, failed int, err error) {
	// A user-cancellable layer wraps the inactivity-timeout layer. The
	// inactivity timeout lets a job that keeps making progress run as long
	// as it needs; only stalled or explicitly cancelled jobs abort.
	userCtx, userCancel := context.WithCancelCause(ctx)
	s.running.Store(job.ID, userCancel)
	unlock := s.lockTitleJob(jobTitleID(job))
	jobCtx, guard, stopStall := stallContext(userCtx, s.jobTimeout)
	err = s.run(jobCtx, cfg, logSvc, job, guard)
	s.running.Delete(job.ID)
	cancelled := errors.Is(context.Cause(userCtx), errJobCancelled)
	if err != nil && !cancelled {
		if cause := context.Cause(jobCtx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = fmt.Errorf("%v: %w", cause, err)
		}
	}
	stopStall()
	unlock()
	userCancel(nil)
	if cancelled {
		if markErr := s.jobs.MarkCancelled(markCtx, job.ID); markErr != nil {
			return 0, 1, markErr
		}
		return 0, 1, nil
	}
	if err != nil {
		if markErr := s.jobs.MarkFailed(markCtx, job.ID, err); markErr != nil {
			return 0, 1, markErr
		}
		return 0, 1, nil
	}
	if err := s.jobs.MarkDone(markCtx, job.ID); err != nil {
		return 1, 0, err
	}
	return 1, 0, nil
}

func (s *JobService) lockTitleJob(titleID int64) func() {
	if titleID <= 0 {
		return func() {}
	}
	v, _ := s.titleLocks.LoadOrStore(titleID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func jobTitleID(job jobs.Job) int64 {
	var payload JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return 0
	}
	return payload.TitleID
}

func (s *JobService) run(ctx context.Context, cfg *config.Config, logSvc ui.Log, job jobs.Job, progress ProgressManager) error {
	var payload JobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("decode job payload: %w", err)
	}

	logSvc = logSvc.With("job_id", job.ID, "job_type", job.Type)
	switch job.Type {
	case jobs.TypeRefreshTitle:
		if payload.TitleID > 0 {
			title, err := s.lib.GetTitle(ctx, payload.TitleID)
			if err != nil {
				return err
			}
			if _, err = s.lib.RefreshTitle(ctx, cfg, logSvc, title); err != nil {
				return err
			}
			if payload.DownloadAfterRefresh {
				return s.enqueueDownloadAfterRefresh(ctx, payload.TitleID)
			}
			return nil
		}
		return s.expandTitleJob(ctx, jobs.TypeRefreshTitle, job.ID)
	case jobs.TypeScanDownloads:
		if payload.TitleID > 0 {
			_, err := s.lib.ScanDownloads(ctx, cfg, payload.TitleID)
			return err
		}
		return s.expandTitleJob(ctx, jobs.TypeScanDownloads, job.ID)
	case jobs.TypeDownloadMissing:
		if payload.TitleID > 0 {
			if payload.ResetFailed {
				if err := s.lib.ResetFailedDownloads(ctx, payload.TitleID); err != nil {
					return err
				}
			}
			title, err := s.lib.GetTitle(ctx, payload.TitleID)
			if err != nil {
				return err
			}
			if title.DiscoveredCount == 0 {
				if _, err := s.lib.RefreshTitle(ctx, cfg, logSvc, title); err != nil {
					return err
				}
			}
			_, err = s.lib.DownloadMissing(ctx, cfg, logSvc, payload.TitleID, progress)
			return err
		}
		return s.expandTitleJob(ctx, jobs.TypeDownloadMissing, job.ID)
	case jobs.TypeDetectCrops:
		if payload.TitleID > 0 {
			return s.detectCrops(ctx, payload.TitleID, logSvc)
		}
		return s.expandDetectCrops(ctx, job.ID)
	case jobs.TypeSyncAniList:
		if payload.UserID > 0 {
			return s.runAniListSync(ctx, payload.UserID, progress)
		}
		return s.expandAniListSync(ctx, job.ID)
	case jobs.TypeCatalogRefresh:
		return s.runCatalogRefresh(ctx, progress)
	case jobs.TypeBackupUserData:
		_, err := s.CreateBackup(ctx)
		return err
	case jobs.TypeAttachVolumes:
		title, err := s.lib.GetTitle(ctx, payload.TitleID)
		if err != nil {
			return err
		}
		return s.lib.AttachVolumesFolder(ctx, cfg, title, payload.Folder)
	case jobs.TypeVerifySource:
		_, err := s.VerifySource(ctx, cfg, logSvc, payload.SourceID)
		return err
	case jobs.TypeMatchSources:
		_, err := s.want.MatchSources(ctx, cfg, logSvc, payload.CatalogID)
		return err
	default:
		return fmt.Errorf("unknown job type %q", job.Type)
	}
}

func (s *JobService) expandTitleJob(ctx context.Context, typ string, parentID int64) error {
	titles, err := s.lib.ListTitles(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	var errs []error
	for _, title := range titles {
		if !globalTitleJobApplies(typ, title, now) {
			continue
		}
		_, err := s.enqueueChild(ctx, typ, JobPayload{TitleID: title.ID}, time.Now(), parentID)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// detectCrops adds content-crop entries to a title's downloaded archives.
func (s *JobService) detectCrops(ctx context.Context, titleID int64, logSvc ui.Log) error {
	chs, err := s.TitleChapters(ctx, titleID)
	if err != nil {
		return err
	}
	added, failed := 0, 0
	for _, ch := range chs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !ch.Downloaded || ch.OutputFile == "" {
			continue
		}
		ok, err := util.AddCropsToCBZ(ch.OutputFile)
		if err != nil {
			failed++
			logSvc.Debugf("detect crops: %s: %v\n", ch.OutputFile, err)
			continue
		}
		if ok {
			added++
		}
	}
	if failed > 0 {
		return fmt.Errorf("crops added to %d archives, %d failed", added, failed)
	}
	return nil
}

// expandDetectCrops fans the global job out per title with downloads,
// skipping titles that are most likely read in strip mode.
func (s *JobService) expandDetectCrops(ctx context.Context, parentID int64) error {
	titles, err := s.lib.ListTitles(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, title := range titles {
		if title.CompletedCount == 0 || s.stripLikely(ctx, title) {
			continue
		}
		_, err := s.enqueueChild(ctx, jobs.TypeDetectCrops, JobPayload{TitleID: title.ID}, time.Now(), parentID)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *JobService) stripLikely(ctx context.Context, title library.Title) bool {
	if title.CatalogMangaID == nil {
		return false
	}
	manga, err := s.want.GetManga(ctx, *title.CatalogMangaID)
	if err != nil {
		return false
	}
	return mangaStripLikely(manga)
}

// mangaStripLikely: KR/CN/TW origin (webtoon/manhua, from the raw AniList
// record) or the Full Color tag.
func mangaStripLikely(m catalog.Manga) bool {
	for _, t := range m.Tags {
		if strings.EqualFold(strings.TrimSpace(t), "full color") {
			return true
		}
	}
	var raw struct {
		CountryOfOrigin string `json:"countryOfOrigin"`
	}
	if json.Unmarshal([]byte(m.RawJSON), &raw) == nil {
		switch raw.CountryOfOrigin {
		case "KR", "CN", "TW":
			return true
		}
	}
	return false
}

func (s *JobService) enqueueDownloadAfterRefresh(ctx context.Context, titleID int64) error {
	title, err := s.lib.GetTitle(ctx, titleID)
	if err != nil {
		return err
	}
	if title.MissingCount == 0 {
		return nil
	}
	if _, err := s.enqueueExact(ctx, jobs.TypeDownloadMissing, JobPayload{TitleID: title.ID}, time.Now()); err != nil {
		return fmt.Errorf("queue download missing: %w", err)
	}
	return nil
}

func titleRefreshDue(title library.Title, now time.Time) bool {
	interval := strings.TrimSpace(title.RefreshInterval)
	if interval == "" {
		return true // no override: follow the global sweep cadence
	}
	d, err := time.ParseDuration(interval)
	if err != nil || d <= 0 || title.LastRefreshedAt == nil {
		return true
	}
	return now.Sub(*title.LastRefreshedAt) >= d
}

func globalTitleJobApplies(typ string, title library.Title, now time.Time) bool {
	switch typ {
	case jobs.TypeRefreshTitle:
		// pending:/local: titles have nothing to refresh from
		return title.Monitored && strings.HasPrefix(title.SourceURL, "http") && titleRefreshDue(title, now)
	case jobs.TypeDownloadMissing:
		return title.Monitored && strings.HasPrefix(title.SourceURL, "http") && (title.MissingCount > 0 || title.DiscoveredCount == 0)
	case jobs.TypeScanDownloads:
		return title.CompletedCount > 0
	default:
		return false
	}
}

func validateJob(typ string, payload JobPayload) error {
	switch typ {
	case jobs.TypeRefreshTitle, jobs.TypeScanDownloads, jobs.TypeDownloadMissing, jobs.TypeDetectCrops:
		if payload.TitleID < 0 {
			return fmt.Errorf("invalid title id %d", payload.TitleID)
		}
	case jobs.TypeSyncAniList, jobs.TypeCatalogRefresh, jobs.TypeBackupUserData:
	case jobs.TypeAttachVolumes:
		if payload.TitleID <= 0 || strings.TrimSpace(payload.Folder) == "" {
			return fmt.Errorf("title id and folder are required")
		}
	case jobs.TypeVerifySource:
		if strings.TrimSpace(payload.SourceID) == "" {
			return fmt.Errorf("source id is required")
		}
	case jobs.TypeMatchSources:
		if payload.CatalogID <= 0 {
			return fmt.Errorf("catalog id is required")
		}
	default:
		return fmt.Errorf("unknown job type %q", typ)
	}

	return nil
}

func titleScopedJob(typ string) bool {
	switch typ {
	case jobs.TypeRefreshTitle, jobs.TypeScanDownloads, jobs.TypeDownloadMissing, jobs.TypeDetectCrops:
		return true
	default:
		return false
	}
}

func activeJobStatus(status string) bool {
	switch status {
	case "queued", "running", "failed":
		return true
	default:
		return false
	}
}
