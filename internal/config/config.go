package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/browserdownload"
	"github.com/brogergvhs/kaodoku/internal/browserfetch"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/util"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Output         string   `yaml:"output"`
	DownloadDir    string   `yaml:"download_dir"`
	ImageWorkers   int      `yaml:"image_workers"`
	ChapterWorkers int      `yaml:"chapter_workers"`
	KeepFolders    bool     `yaml:"keep_folders"`
	Debug          bool     `yaml:"debug"`
	LogFormat      string   `yaml:"log_format"`
	AllowExt       []string `yaml:"allow_ext"`
	Languages      []string `yaml:"languages"`
	LanguageMode   string   `yaml:"-"`
	Chapterless    bool     `yaml:"-"`

	DefaultURL          string `yaml:"default_url"`
	DefaultRange        string `yaml:"default_range"`
	DefaultExcludeRange string `yaml:"default_exclude_range"`
	DefaultList         string `yaml:"default_list"`
	DefaultExcludeList  string `yaml:"default_exclude_list"`
	CheckJS             bool   `yaml:"check_js"`

	Cookie     string `yaml:"cookie"`
	CookieFile string `yaml:"cookie_file"`
	UserAgent  string `yaml:"user_agent"`

	SkipBroken bool `yaml:"skip_broken"`

	RateLimit       RateLimitConfig       `yaml:"rate_limit"`
	BrowserSolver   BrowserSolverConfig   `yaml:"browser_solver"`
	BrowserDownload BrowserDownloadConfig `yaml:"browser_downloader"`
	KotatsuWorker   KotatsuWorkerConfig   `yaml:"kotatsu_worker"`
	CookieDBPath    string                `yaml:"-"`
}

type KotatsuWorkerConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
	APIKey   string `yaml:"api_key"`
}

// RateLimitConfig controls per-host request pacing. Zero values use the
// built-in default (200ms interval, burst 2).
type RateLimitConfig struct {
	Disabled   bool `yaml:"disabled"`
	IntervalMS int  `yaml:"interval_ms"`
	Burst      int  `yaml:"burst"`
}

type BrowserSolverConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Provider       string `yaml:"provider"`
	Endpoint       string `yaml:"endpoint"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type BrowserDownloadConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Endpoint       string `yaml:"endpoint"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type Options struct {
	IgnoreConfig        bool
	Debug               bool
	Output              string
	DownloadDir         string
	ImageWorkers        int
	ChapterWorkers      int
	KeepFolders         bool
	DefaultURL          string
	DefaultRange        string
	DefaultExcludeRange string
	DefaultList         string
	DefaultExcludeList  string
	CheckJS             bool
	Cookie              string
	CookieFile          string
	UserAgent           string
	SkipBroken          bool
	BrowserSolver       BrowserSolverConfig
	BrowserDownload     BrowserDownloadConfig
}

func DefaultConfig() *Config {
	return &Config{
		Output:              ".",
		DownloadDir:         ".",
		ImageWorkers:        5,
		ChapterWorkers:      2,
		KeepFolders:         false,
		Debug:               false,
		LogFormat:           "text",
		DefaultURL:          "",
		DefaultRange:        "",
		DefaultExcludeRange: "",
		DefaultList:         "",
		DefaultExcludeList:  "",
		Cookie:              "",
		CookieFile:          "",
		UserAgent:           "",
		CheckJS:             false,
		SkipBroken:          false,
		AllowExt:            []string{"jpg", "jpeg", "png", "webp"},
		BrowserSolver: BrowserSolverConfig{
			Enabled:        false,
			Provider:       browserfetch.ProviderFlareSolverr,
			Endpoint:       browserfetch.DefaultFlareSolverrEndpoint,
			TimeoutSeconds: 60,
		},
		BrowserDownload: BrowserDownloadConfig{
			Enabled:        false,
			Endpoint:       browserdownload.DefaultEndpoint,
			TimeoutSeconds: 180,
		},
		KotatsuWorker: KotatsuWorkerConfig{
			Enabled:  false,
			Endpoint: kotatsu.DefaultEndpoint,
		},
	}
}

func SaveYAML(cfg *Config, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return util.WriteFileAtomic(path, data, 0644)
}

func loadYAML(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}

	return &c, nil
}

func LoadMerged(opts Options) (*Config, string, error) {
	if opts.IgnoreConfig {
		cfg := DefaultConfig()
		mergeConfig(cfg, opts)
		normalizeDefaults(cfg)
		return cfg, "(ignored config)", nil
	}

	activePath, err := ActiveConfigPath()
	if err == ErrNoConfig || activePath == "" {
		cfg := DefaultConfig()
		mergeConfig(cfg, opts)
		normalizeDefaults(cfg)
		return cfg, "(default config in memory)\nRun `kaodoku config init` to create an actual config\n", nil
	}
	if err != nil {
		return nil, "", err
	}

	cfg, err := loadYAML(activePath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load config %s: %w", activePath, err)
	}

	mergeConfig(cfg, opts)
	normalizeDefaults(cfg)

	return cfg, activePath, nil
}

func mergeConfig(c *Config, o Options) {
	if o.Output != "" {
		c.Output = o.Output
	}
	if o.DownloadDir != "" {
		c.DownloadDir = o.DownloadDir
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_DOWNLOAD_DIR")); env != "" {
		c.DownloadDir = env
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_DOWNLOADER_ENABLED")); env != "" {
		if enabled, err := strconv.ParseBool(env); err == nil {
			c.BrowserDownload.Enabled = enabled
		}
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_DOWNLOADER_ENDPOINT")); env != "" {
		c.BrowserDownload.Endpoint = env
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_DOWNLOADER_TIMEOUT_SECONDS")); env != "" {
		if seconds, err := strconv.Atoi(env); err == nil {
			c.BrowserDownload.TimeoutSeconds = seconds
		}
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_KOTATSU_WORKER_ENABLED")); env != "" {
		if enabled, err := strconv.ParseBool(env); err == nil {
			c.KotatsuWorker.Enabled = enabled
		}
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_KOTATSU_WORKER_ENDPOINT")); env != "" {
		c.KotatsuWorker.Endpoint = env
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_KOTATSU_WORKER_API_KEY")); env != "" {
		c.KotatsuWorker.APIKey = env
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_SOLVER_ENABLED")); env != "" {
		if enabled, err := strconv.ParseBool(env); err == nil {
			c.BrowserSolver.Enabled = enabled
		}
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_SOLVER_PROVIDER")); env != "" {
		c.BrowserSolver.Provider = env
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_SOLVER_ENDPOINT")); env != "" {
		c.BrowserSolver.Endpoint = env
	}
	if env := strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_SOLVER_TIMEOUT_SECONDS")); env != "" {
		if seconds, err := strconv.Atoi(env); err == nil {
			c.BrowserSolver.TimeoutSeconds = seconds
		}
	}
	if o.ImageWorkers != 0 {
		c.ImageWorkers = o.ImageWorkers
	}
	if o.ChapterWorkers != 0 {
		c.ChapterWorkers = o.ChapterWorkers
	}
	if o.KeepFolders {
		c.KeepFolders = true
	}
	if o.Debug {
		c.Debug = true
	}
	if o.DefaultURL != "" {
		c.DefaultURL = o.DefaultURL
	}
	if o.DefaultRange != "" {
		c.DefaultRange = o.DefaultRange
	}
	if o.DefaultExcludeRange != "" {
		c.DefaultExcludeRange = o.DefaultExcludeRange
	}
	if o.DefaultList != "" {
		c.DefaultList = o.DefaultList
	}
	if o.DefaultExcludeList != "" {
		c.DefaultExcludeList = o.DefaultExcludeList
	}
	if o.CheckJS {
		c.CheckJS = true
	}
	if o.Cookie != "" {
		c.Cookie = o.Cookie
	}
	if o.CookieFile != "" {
		c.CookieFile = o.CookieFile
	}
	if o.UserAgent != "" {
		c.UserAgent = o.UserAgent
	}
	if o.SkipBroken {
		c.SkipBroken = true
	}
	if o.BrowserSolver.Enabled {
		c.BrowserSolver.Enabled = true
	}
	if o.BrowserSolver.Provider != "" {
		c.BrowserSolver.Provider = o.BrowserSolver.Provider
	}
	if o.BrowserSolver.Endpoint != "" {
		c.BrowserSolver.Endpoint = o.BrowserSolver.Endpoint
	}
	if o.BrowserSolver.TimeoutSeconds != 0 {
		c.BrowserSolver.TimeoutSeconds = o.BrowserSolver.TimeoutSeconds
	}
	if o.BrowserDownload.Enabled {
		c.BrowserDownload.Enabled = true
	}
	if o.BrowserDownload.Endpoint != "" {
		c.BrowserDownload.Endpoint = o.BrowserDownload.Endpoint
	}
	if o.BrowserDownload.TimeoutSeconds != 0 {
		c.BrowserDownload.TimeoutSeconds = o.BrowserDownload.TimeoutSeconds
	}
}

func normalizeDefaults(c *Config) {
	if c.Output == "" {
		c.Output = "."
	}
	if c.DownloadDir == "" {
		c.DownloadDir = "."
	}
	if c.ImageWorkers == 0 {
		c.ImageWorkers = 5
	}
	if c.ChapterWorkers == 0 {
		c.ChapterWorkers = 2
	}
	if c.BrowserSolver.Provider == "" {
		c.BrowserSolver.Provider = browserfetch.ProviderFlareSolverr
	}
	if c.BrowserSolver.Endpoint == "" {
		c.BrowserSolver.Endpoint = browserfetch.DefaultFlareSolverrEndpoint
	}
	if c.BrowserSolver.TimeoutSeconds == 0 {
		c.BrowserSolver.TimeoutSeconds = 60
	}
	if c.KotatsuWorker.Endpoint == "" {
		c.KotatsuWorker.Endpoint = kotatsu.DefaultEndpoint
	}
	if c.BrowserDownload.Endpoint == "" {
		c.BrowserDownload.Endpoint = browserdownload.DefaultEndpoint
	}
	if c.BrowserDownload.TimeoutSeconds == 0 {
		c.BrowserDownload.TimeoutSeconds = 180
	}
}

func (c *Config) Print() {
	if c.Output != "" {
		fmt.Printf(" -output: %s\n", c.Output)
	}
	if c.DownloadDir != "" {
		fmt.Printf(" -download_dir: %s\n", c.DownloadDir)
	}
	fmt.Printf(" -image_workers: %d\n", c.ImageWorkers)
	fmt.Printf(" -chapter_workers: %d\n", c.ChapterWorkers)
	if c.KeepFolders {
		fmt.Printf(" -keep_folders: %t\n", c.KeepFolders)
	}
	if c.Debug {
		fmt.Printf(" -debug: %t\n", c.Debug)
	}
	if c.DefaultURL != "" {
		fmt.Printf(" -url: %s\n", c.DefaultURL)
	}
	if c.DefaultRange != "" {
		fmt.Printf(" -range: %s\n", c.DefaultRange)
	}
	if c.DefaultExcludeRange != "" {
		fmt.Printf(" -exclude_range: %s\n", c.DefaultExcludeRange)
	}
	if c.DefaultList != "" {
		fmt.Printf(" -list: %s\n", c.DefaultList)
	}
	if c.DefaultExcludeList != "" {
		fmt.Printf(" -exclude_list: %s\n", c.DefaultExcludeList)
	}
	if c.CheckJS {
		fmt.Printf(" -check_js: %t\n", c.CheckJS)
	}
	if c.Cookie != "" {
		fmt.Printf(" -cookie: (set, %d chars)\n", len(c.Cookie))
	}
	if c.CookieFile != "" {
		fmt.Printf(" -cookie_file: %s\n", c.CookieFile)
	}
	if c.SkipBroken {
		fmt.Printf(" -skip_broken: %t\n", c.SkipBroken)
	}
	if len(c.AllowExt) > 0 {
		fmt.Printf(" -allow_ext: %s\n", strings.Join(c.AllowExt, ", "))
	}
	if c.BrowserSolver.Enabled {
		fmt.Printf(" -browser_solver: %s %s timeout=%ds\n", c.BrowserSolver.Provider, c.BrowserSolver.Endpoint, c.BrowserSolver.TimeoutSeconds)
	}
	if c.BrowserDownload.Enabled {
		fmt.Printf(" -browser_downloader: %s timeout=%ds\n", c.BrowserDownload.Endpoint, c.BrowserDownload.TimeoutSeconds)
	}
}
