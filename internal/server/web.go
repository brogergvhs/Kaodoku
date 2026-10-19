package server

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers/registry"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/util"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

type webUI struct {
	svc      *service.JobService
	tmpl     *template.Template
	kick     func() // start the job runner now, non-blocking
	assetVer string
}

type pageData struct {
	Title, Nav   string
	User         *auth.User
	Theme        string
	ThemeCSS     template.CSS // custom-theme variable overrides
	Screens      []library.Screen
	ActiveID     int64
	PendingUsers int
	Content      template.HTML
}
type readerView struct {
	PrevURL      string
	Title        library.Title
	Manifest     readerManifestResponse
	ManifestJSON template.JS // consumed by the reader script to build the strip
	Empty        string
	NextURL      string
	PagePosition string
}
type dashData struct {
	User *auth.User
}
type libraryView struct {
	Controls           libraryControls
	Table              libraryResults
	Screen             *library.Screen
	ShowEditor         bool
	OpenEditor         bool
	TagOptions         []catalog.ContentTag
	IncludeTags        []string
	ExcludeTags        []string
	Cfg                library.ScreenConfig
	CollectionName     string
	CollectionIDs      string
	CollectionID       int64
	CollectionSmartKey string
	SourceOptions      []sources.Source
	AllowNSFW          bool
}
type libraryControls struct {
	Q           string
	Monitor     string
	Fav         string
	NSFW        string
	Sources     []string
	Progress    string
	Content     string // chapters/volumes presence
	Sort        string
	Dir         string
	View        string
	IncludeTags []string
	ExcludeTags []string
}

// SourceSet reports which source tokens are selected, for the filter template.
func (c libraryControls) SourceSet() map[string]bool {
	set := make(map[string]bool, len(c.Sources))
	for _, s := range c.Sources {
		set[s] = true
	}
	return set
}

type healthView struct {
	Services []service.ServiceHealth
	Interval string // HTMX poll interval, e.g. "60s"
}
type activityView struct {
	Title         library.Title
	Manga         catalog.Manga
	Sources       matchView
	LinkedSources []linkedSourceView // sources already linked to this title
	SingleSources []sources.Source   // single-manga sources selectable for linking
	LinkSources   []sources.Source   // searchable sources for specifying a page URL
	RefreshEvery  string             // effective global refresh cadence
	Running       map[string]bool    // job type -> active (for button locking)
	ActiveLabel   string
	Queued        []string
	Failed        bool
	Error         string
	User          *auth.User
	AniList       service.AniListConnection
	Content       titleContentView
	Screen        int64
}
type sourceRowView struct {
	Source     sources.Source
	Active     bool
	Poll       bool
	Titles     int // library titles using this source
	FetchFails int // titles whose last chapter refresh failed
}
type sourceProbeView struct {
	TitleID  int64
	SourceID string
	URL      string
	Result   service.SourceTestResult
}
type linkedSourceView struct {
	Name     string
	SourceID string
	URL      string
	Active   bool // the source used for refresh/download
}
type matchView struct {
	DomID     string
	PollURL   string
	Matches   []catalog.Match
	Active    bool
	Failed    bool
	Error     string
	TitleID   int64  // link target for "Use this source"
	LinkedURL string // the title's current source URL (marked as linked)
}

func titleSourceView(title library.Title, active, failed bool, msg string, matches []catalog.Match) matchView {
	return matchView{
		DomID:   fmt.Sprintf("sources-%d", title.ID),
		PollURL: fmt.Sprintf("/ui/library/%d/sources", title.ID),
		Active:  active, Failed: failed, Error: msg, Matches: matches,
		TitleID: title.ID, LinkedURL: title.SourceURL,
	}
}

type settingsView struct {
	Groups        []settingGroup
	AniList       service.AniListConnection
	RedirectURL   string
	AppConfigured bool
}
type settingGroup struct {
	Title  string
	Fields []settingField
}
type settingField struct {
	Key, Label, Desc, Value string
	Kind                    string // "", "select", "color"
	Options                 []string
}

// settingMeta maps a technical setting key to a human label and description.
func settingMeta(key string) (label, desc string) {
	if token, ok := strings.CutPrefix(key, "ui.custom."); ok {
		return token, ""
	}
	switch key {
	case service.SettingServeAniListSyncEvery:
		return "AniList sync", "How often connected accounts sync reading progress with AniList (e.g. 12h). Empty disables."
	case service.SettingServeCatalogEvery:
		return "Catalog refresh", "How often cached AniList metadata (tags, release status) is re-fetched for tracked titles. Empty disables."
	case service.SettingRateLimitIntervalMS:
		return "Request pacing (ms)", "Minimum milliseconds between page requests to the same site. Image downloads are exempt. Lower is faster; too low risks bans."
	case service.SettingRateLimitBurst:
		return "Request pacing burst", "How many page requests may go out back-to-back before pacing kicks in."
	case service.SettingRateLimitDisabled:
		return "Disable request pacing", "true turns off per-site pacing entirely (not recommended for Cloudflare-fronted sites)."
	case service.SettingAniListClientID:
		return "AniList client ID", "From your AniList developer application."
	case service.SettingAniListClientSecret:
		return "AniList client secret", "Kept server-side; used to exchange login codes for tokens."
	case service.SettingUITheme:
		return "Theme", "Interface color theme. Pick custom to use the colors below."
	case service.SettingServeRefreshEvery:
		return "Check for new chapters", "How often each tracked manga's source is checked for newly released chapters (e.g. 1h, 30m)."
	case service.SettingServeScanEvery:
		return "Re-scan downloaded files", "How often the download folders are re-checked to reconcile which chapters are already on disk."
	case service.SettingServeDownloadEvery:
		return "Download missing chapters", "How often missing chapters of monitored manga are downloaded automatically."
	case service.SettingServeSourceVerifyEvery:
		return "Verify sources", "How often enabled source definitions are re-checked against their sites. Use 0 to disable."
	case service.SettingServeBackupEvery:
		return "Back up user data", "How often user data is backed up. User data only: progress, settings, users, roles, library metadata, collections and sources. Empty disables scheduled backups."
	case service.SettingServeRunEvery:
		return "Background task interval", "How often the background worker wakes to run queued jobs. Lower is more responsive, higher is less busy."
	case service.SettingBrowserSolverEnabled:
		return "Use a browser solver for protected sites", "Enable a headless browser / Cloudflare solver for sources that block plain requests (true or false)."
	case service.SettingBrowserSolverProvider:
		return "Solver type", "Which solver to use for protected sites (e.g. flaresolverr)."
	case service.SettingBrowserSolverEndpoint:
		return "Solver address", "The URL where your solver (e.g. FlareSolverr) is reachable."
	case service.SettingBrowserSolverTimeoutSeconds:
		return "Solver timeout (seconds)", "How long to wait for the solver to load a page before giving up."
	case service.SettingBrowserDownloaderEnabled:
		return "Use browser image downloader", "Enable the Selenium browser worker fallback for sources whose images only load in a browser (true or false)."
	case service.SettingBrowserDownloaderEndpoint:
		return "Browser downloader address", "The URL where the browser downloader worker is reachable."
	case service.SettingBrowserDownloaderTimeoutSeconds:
		return "Browser downloader timeout (seconds)", "How long to wait for browser-captured chapter downloads before giving up."
	case service.SettingSourceRegistryURL:
		return "Extra source list URL", "Optional URL to load additional scraper definitions from. Leave blank to use built-in sources."
	case service.SettingJobsMaxAttempts:
		return "Job retry limit", "How many times a failed background job (refresh, scan) is retried before it is given up."
	case service.SettingJobsTimeout:
		return "Job stall limit", "How long a background job may go without making progress before it is aborted (e.g. 10m). Jobs that keep downloading run as long as they need."
	case service.SettingJobsWorkers:
		return "Job worker count", "How many background jobs can run at the same time. Jobs for the same manga still run one at a time."
	case service.SettingJobsKeepHistory:
		return "Keep finished jobs", "How long completed, cancelled and dead jobs stay in the jobs list before they are deleted (e.g. 720h). 0 or empty keeps them forever. Queued, running and retrying jobs are never deleted."
	case service.SettingDownloadsMaxAttempts:
		return "Download retry limit", "How many times a failed chapter download is retried before giving up."
	case service.SettingServicesHealthInterval:
		return "Service health check interval", "How often the dashboard re-checks that FlareSolverr and the browser downloader are reachable (e.g. 60s, 2m)."
	}
	return key, ""
}

type toastView struct {
	OK  bool
	Msg string
}

// staticAssetVersion hashes the embedded static files so asset URLs change
// with every build that touches them.
func staticAssetVersion() string {
	h := fnv.New64a()
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := staticFS.ReadFile(path)
		if err == nil {
			_, _ = h.Write([]byte(path))
			_, _ = h.Write(data)
		}
		return nil
	})
	return strconv.FormatUint(h.Sum64(), 36)
}

// registerUI mounts the server-rendered UI and its HTMX endpoints on mux.
func registerUI(mux *http.ServeMux, svc *service.JobService, runJobs func(context.Context) (service.RunSummary, error)) {
	u := &webUI{svc: svc, kick: func() {
		if runJobs != nil {
			go func() { _, _ = runJobs(context.Background()) }()
		}
	}}
	u.assetVer = staticAssetVersion()
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	static, _ := fs.Sub(staticFS, "static")
	// Assets are addressed with a content-hash query (?v=...), so they can be
	// cached hard; a new build changes the URL and busts stale copies (embedded
	// files carry no modtime, so browsers would otherwise cache heuristically).
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		staticHandler.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /collections", u.collectionsPage)
	mux.HandleFunc("GET /collections/view", u.collectionMembersPage)
	mux.HandleFunc("GET /{$}", u.homePage)
	mux.HandleFunc("GET /management", u.management)
	mux.HandleFunc("GET /metrics", u.metricsPage)
	mux.HandleFunc("GET /ui/metrics", u.metricsBody)
	mux.HandleFunc("GET /search", u.searchPage)
	mux.HandleFunc("GET /library", u.libraryPage)
	mux.HandleFunc("GET /library/{id}", u.titlePage)
	mux.HandleFunc("GET /manga/{id}", u.catalogMangaPage)
	mux.HandleFunc("GET /reader/{id}", u.readerPage)
	mux.HandleFunc("GET /import", u.importPage)
	mux.HandleFunc("GET /sources", u.sourcesPage)
	mux.HandleFunc("GET /backups", u.backupsPage)
	mux.HandleFunc("GET /settings", u.settingsPage)

	mux.HandleFunc("POST /ui/search", u.search)
	mux.HandleFunc("POST /ui/library/add", u.addToLibrary)
	mux.HandleFunc("POST /ui/library/bulk", u.libraryBulk)
	mux.HandleFunc("POST /ui/library/{id}/refresh", u.libAction(jobs.TypeRefreshTitle, "refreshing"))
	mux.HandleFunc("POST /ui/library/{id}/download", u.libAction(jobs.TypeDownloadMissing, "downloading"))
	mux.HandleFunc("POST /ui/library/{id}/crops", u.libAction(jobs.TypeDetectCrops, "detecting crops"))
	mux.HandleFunc("POST /ui/library/{id}/scan", u.libAction(jobs.TypeScanDownloads, "scanning"))
	mux.HandleFunc("GET /ui/library/{id}/activity", u.libActivity)
	mux.HandleFunc("POST /ui/library/{id}/monitored", u.libMonitored)
	mux.HandleFunc("POST /ui/library/{id}/favourite", u.libFavourite)
	mux.HandleFunc("POST /ui/library/{id}/language-mode", u.libLanguageMode)
	mux.HandleFunc("POST /ui/library/{id}/refresh-interval", u.libRefreshInterval)
	mux.HandleFunc("POST /ui/library/{id}/remove", u.libRemove)
	mux.HandleFunc("GET /ui/library/{id}/remove-dialog", u.titleRemoveDialog)
	mux.HandleFunc("GET /ui/library/{id}/settings-dialog", u.titleSettingsDialog)
	mux.HandleFunc("POST /ui/library/{id}/find-sources", u.findSources)
	mux.HandleFunc("GET /ui/library/{id}/sources", u.titleSources)
	mux.HandleFunc("GET /ui/library/{id}/chapters", u.chaptersTable)
	mux.HandleFunc("GET /ui/library/{id}/chapters/download", u.chaptersDownload)
	mux.HandleFunc("GET /ui/library/{id}/chapters/{chapterID}/download", u.chapterDownload)
	mux.HandleFunc("POST /ui/screens", u.screenSave)
	mux.HandleFunc("POST /ui/screens/{id}", u.screenSave)
	mux.HandleFunc("POST /ui/screens/{id}/delete", u.screenDelete)
	mux.HandleFunc("POST /ui/screens/reorder", u.screenReorder)
	mux.HandleFunc("POST /ui/collections", u.collectionCreate)
	mux.HandleFunc("POST /ui/collections/{id}/rename", u.collectionRename)
	mux.HandleFunc("POST /ui/collections/{id}/delete", u.collectionDelete)
	mux.HandleFunc("POST /ui/collections/{id}/remove", u.collectionRemoveMember)
	mux.HandleFunc("GET /ui/collections/{id}/manage", u.collectionManage)
	mux.HandleFunc("GET /ui/collections/smart/{key}/manage", u.smartCollectionManage)
	mux.HandleFunc("POST /ui/collections/smart/{key}/remove", u.smartCollectionRemovePin)
	mux.HandleFunc("GET /ui/library/{id}/collections", u.libCollectionsDialog)
	mux.HandleFunc("POST /ui/library/{id}/collections/add", u.libAddToCollection)
	mux.HandleFunc("GET /ui/library/{id}/content", u.titleContentFrag)
	mux.HandleFunc("POST /ui/library/{id}/volumes/range", u.volumesRange)
	mux.HandleFunc("POST /ui/volumes/{id}/read", u.volumeRead(true))
	mux.HandleFunc("POST /ui/volumes/{id}/unread", u.volumeRead(false))
	mux.HandleFunc("GET /ui/volumes/{id}/cover", u.volumeCover)
	mux.HandleFunc("POST /ui/volumes/{id}/cover", u.volumeCoverUpload)
	mux.HandleFunc("POST /ui/volumes/{id}/cover/reset", u.volumeCoverReset)
	mux.HandleFunc("POST /ui/titles/{id}/cover", u.titleCoverUpload)
	mux.HandleFunc("POST /ui/titles/{id}/cover/reset", u.titleCoverReset)
	mux.HandleFunc("POST /ui/import/attach-volumes", u.importAttachVolumes)
	mux.HandleFunc("GET /ui/library/{id}/progress", u.titleProgress)
	mux.HandleFunc("POST /ui/library/{id}/chapters/{chapterID}/read", u.chapterRead(true))
	mux.HandleFunc("POST /ui/library/{id}/chapters/{chapterID}/unread", u.chapterRead(false))
	mux.HandleFunc("POST /ui/library/{id}/chapters/{chapterID}/remove", u.chapterRemove)
	mux.HandleFunc("GET /ui/library/{id}/chapters/{chapterID}/rename-dialog", u.chapterRenameDialog)
	mux.HandleFunc("POST /ui/library/{id}/chapters/{chapterID}/rename", u.chapterRename)
	mux.HandleFunc("POST /ui/library/{id}/chapters/{chapterID}/reset-name", u.chapterResetName)
	mux.HandleFunc("POST /ui/library/{id}/chapters/reset-names", u.chapterResetNames)
	mux.HandleFunc("GET /ui/library/{id}/chapters/bulk-rename", u.chapterBulkRenameForm)
	mux.HandleFunc("POST /ui/library/{id}/chapters/bulk-rename/preview", u.chapterBulkRenamePreview)
	mux.HandleFunc("POST /ui/library/{id}/chapters/bulk-rename", u.chapterBulkRenameApply)
	mux.HandleFunc("POST /ui/library/{id}/chapters/range", u.chapterRangeRead)
	mux.HandleFunc("POST /ui/library/{id}/chapters/delete-range", u.chapterDeleteRange)
	mux.HandleFunc("POST /ui/library/{id}/link", u.linkSource)
	mux.HandleFunc("POST /ui/library/{id}/link-source", u.linkSourceByID)
	mux.HandleFunc("POST /ui/library/{id}/verify-source", u.srcVerifyURL)
	mux.HandleFunc("POST /ui/library/{id}/link-source-url", u.linkSourceURL)
	mux.HandleFunc("POST /ui/library/{id}/unlink-source", u.unlinkSource)
	mux.HandleFunc("POST /ui/import/{folder}/search", u.importSearch)
	mux.HandleFunc("POST /ui/import", u.importDo)
	mux.HandleFunc("POST /ui/sources/{id}/verify", u.srcVerify)
	mux.HandleFunc("GET /ui/sources/{id}/edit", u.srcEdit)
	mux.HandleFunc("POST /ui/sources/{id}/edit", u.srcEditSave)
	mux.HandleFunc("GET /ui/sources/{id}/row", u.srcRow)
	mux.HandleFunc("GET /ui/sources/{id}/title-errors", u.srcTitleErrors)
	mux.HandleFunc("POST /ui/sources/{id}/enabled", u.srcEnabled)
	mux.HandleFunc("POST /ui/sources/{id}/delete", u.srcDelete)
	mux.HandleFunc("POST /ui/sources/sync", u.srcSync)
	mux.HandleFunc("GET /ui/sources/table", u.sourcesTable)
	mux.HandleFunc("POST /ui/sources/test", u.srcTest)
	mux.HandleFunc("POST /ui/sources/custom", u.srcAddCustom)
	mux.HandleFunc("GET /ui/sources/kotatsu", u.kotatsuModal)
	mux.HandleFunc("GET /ui/sources/kotatsu/list", u.kotatsuBrowse)
	mux.HandleFunc("GET /ui/sources/kotatsu/table", u.kotatsuTable)
	mux.HandleFunc("GET /ui/sources/kotatsu/section", u.kotatsuSection)
	mux.HandleFunc("POST /ui/sources/kotatsu/import", u.kotatsuImport)
	mux.HandleFunc("GET /ui/library/table", u.libraryTable)
	mux.HandleFunc("GET /ui/jobs/table", u.jobsTable)
	mux.HandleFunc("POST /ui/jobs/start", u.jobStart)
	mux.HandleFunc("POST /ui/jobs/{id}/cancel", u.jobCancel)
	mux.HandleFunc("GET /ui/backups", u.backupsFrag)
	mux.HandleFunc("POST /ui/backups", u.backupCreate)
	mux.HandleFunc("POST /ui/backups/upload", u.backupUpload)
	mux.HandleFunc("GET /ui/backups/{name}/download", u.backupDownload)
	mux.HandleFunc("POST /ui/backups/{name}/restore", u.backupRestore)
	mux.HandleFunc("POST /ui/backups/{name}/delete", u.backupDelete)
	mux.HandleFunc("GET /ui/sessions", u.sessionsFrag)
	mux.HandleFunc("GET /ui/health", u.health)
	mux.HandleFunc("GET /ui/import/candidates", u.importCandidates)
	mux.HandleFunc("GET /ui/import/titles", u.importTitleOptions)
	mux.HandleFunc("GET /ui/library/{id}/related", u.relatedManga)
	mux.HandleFunc("GET /ui/search/trending", u.trendingManga)
	mux.HandleFunc("GET /anilist/connect", u.anilistConnect)
	mux.HandleFunc("GET /anilist/callback", u.anilistCallback)
	mux.HandleFunc("POST /ui/anilist/disconnect", u.anilistDisconnect)
	mux.HandleFunc("GET /ui/anilist/library", u.anilistLibrary)
	mux.HandleFunc("POST /ui/anilist/sync", u.anilistSyncNow)
	mux.HandleFunc("GET /ui/anilist/suggestions", u.anilistSuggestions)
	mux.HandleFunc("POST /ui/library/{id}/anilist-sync", u.anilistSyncTitle)
	mux.HandleFunc("GET /ui/account", u.accountFrag)
	mux.HandleFunc("POST /ui/account/password", u.accountPassword)
	mux.HandleFunc("POST /ui/account/sessions/revoke", u.accountRevokeSessions)
	mux.HandleFunc("POST /ui/account/tokens", u.accountTokenCreate)
	mux.HandleFunc("POST /ui/account/tokens/{id}/delete", u.accountTokenDelete)
	mux.HandleFunc("GET /users", u.usersPage)
	mux.HandleFunc("GET /ui/users", u.usersFrag)
	mux.HandleFunc("POST /ui/users", u.userCreate)
	mux.HandleFunc("GET /ui/users/{id}/edit", u.userEditModal)
	mux.HandleFunc("GET /ui/users/roles/{id}/edit", u.roleEditModal)
	mux.HandleFunc("POST /ui/users/{id}", u.userUpdate)
	mux.HandleFunc("POST /ui/users/{id}/delete", u.userDelete)
	mux.HandleFunc("POST /ui/users/{id}/approve", u.userApprove)
	mux.HandleFunc("POST /ui/users/roles", u.roleSave)
	mux.HandleFunc("POST /ui/users/roles/{id}/delete", u.roleDelete)
	mux.HandleFunc("PUT /ui/settings", u.settingsSave)
}

// --- rendering ---

func (u *webUI) page(w http.ResponseWriter, r *http.Request, content, title string, data any) {
	var buf bytes.Buffer
	if err := u.tmpl.ExecuteTemplate(&buf, content, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	theme, css := u.theme(r.Context())
	var screens []library.Screen
	if userFrom(r.Context()).Can(auth.PermLibraryView) {
		screens, _ = u.svc.Screens(r.Context())
	}
	activeID, _ := strconv.ParseInt(r.URL.Query().Get("screen"), 10, 64)
	pending := 0
	if viewer := userFrom(r.Context()); viewer.Can(auth.PermUsersApprove) || viewer.Can(auth.PermUsersManage) {
		pending, _ = u.svc.Auth().CountPendingUsers(r.Context())
	}
	if err := u.tmpl.ExecuteTemplate(w, "layout.html", pageData{Title: title, Nav: navFor(r.URL.Path), User: userFrom(r.Context()), Theme: theme, ThemeCSS: css, Screens: screens, ActiveID: activeID, PendingUsers: pending, Content: template.HTML(buf.String())}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type mangaResults struct {
	Heading       string
	View          string // "cards" (default) or "table"
	Items         []searchResultView
	CanAdd        bool
	ShowRelations bool
}

// resultView picks the search results layout from the request.
func resultView(r *http.Request) string {
	switch v := r.FormValue("view"); v {
	case "table", "full":
		return v
	default:
		return "cards"
	}
}

func (u *webUI) mangaResultsView(ctx context.Context, heading, view string, items []catalog.Manga) mangaResults {
	user := userFrom(ctx)
	return mangaResults{Heading: heading, View: view, Items: u.stripItems(ctx, items), CanAdd: user.Can(auth.PermLibraryAdd)}
}

func (u *webUI) stripItems(ctx context.Context, items []catalog.Manga) []searchResultView {
	return stripItems(ctx, u.svc, items)
}

// stripItems guards manga for the acting user and maps tracked ones to their
// library title id.
func stripItems(ctx context.Context, svc *service.JobService, items []catalog.Manga) []searchResultView {
	inLibrary, _ := svc.TitlesByProvider(ctx, catalog.AniListProvider)
	views := make([]searchResultView, 0, len(items))
	for _, m := range items {
		if !contentAllowed(ctx, m.IsAdult, mangaContentTags(m)) {
			continue
		}
		views = append(views, searchResultView{Manga: m, TitleID: inLibrary[m.ProviderID]})
	}
	return views
}

type catalogMangaView struct {
	Manga  catalog.Manga
	CanAdd bool
}

// catalogMangaPage shows an AniList manga that is not (yet) in the library:
// cover, title, description and stats, with a single Add to Library action.
func (u *webUI) catalogMangaPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid := r.PathValue("id")
	if inLib, _ := u.svc.TitlesByProvider(ctx, catalog.AniListProvider); inLib[pid] > 0 {
		http.Redirect(w, r, fmt.Sprintf("/library/%d", inLib[pid]), http.StatusSeeOther)
		return
	}
	id, err := strconv.Atoi(pid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := u.svc.CatalogMangaByAniList(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !contentAllowed(ctx, m.IsAdult, mangaContentTags(m)) {
		http.Error(w, "this title is not available for your account", http.StatusForbidden)
		return
	}
	u.page(w, r, "catalogManga", mangaTitle(m), catalogMangaView{Manga: m, CanAdd: userFrom(ctx).Can(auth.PermLibraryAdd)})
}

func (u *webUI) relatedManga(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil || title.CatalogMangaID == nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		u.frag(w, "mangaResults", mangaResults{})
		return
	}
	items, err := u.svc.RelatedManga(r.Context(), *title.CatalogMangaID, 12)
	if err != nil {
		u.frag(w, "mangaResults", mangaResults{})
		return
	}
	view := u.mangaResultsView(r.Context(), "", "cards", items)
	view.ShowRelations = true
	u.frag(w, "mangaResults", view)
}

func (u *webUI) trendingManga(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c := searchControlsFrom(r.Form)
	limit := 18
	if c.Sort != "" {
		limit = 54 // sort over the full cached pool, not an arbitrary cut
	}
	items, err := u.svc.RecommendedManga(r.Context(), limit)
	if err != nil {
		u.frag(w, "mangaResults", u.guardedBrowse(r.Context(), c))
		return
	}
	items = allowedManga(r.Context(), filterSortManga(items, c))
	if len(items) > 18 {
		items = items[:18]
	}
	view := u.mangaResultsView(r.Context(), "", c.View, items)
	if len(view.Items) == 0 {
		view = u.guardedBrowse(r.Context(), c)
	}
	u.frag(w, "mangaResults", view)
}

// guardedBrowse fills an empty suggestions grid with a popularity browse
// pre-filtered by the acting user's tag guards.
func (u *webUI) guardedBrowse(ctx context.Context, c searchControls) mangaResults {
	items := guardedBrowseManga(ctx, u.svc, c.Sort, c.Dir)
	return u.mangaResultsView(ctx, "", c.View, items)
}

// guardedBrowseManga is a popularity browse pre-filtered by the acting user's
// tag guards, shared by the web and v1 trending fallbacks.
func guardedBrowseManga(ctx context.Context, svc *service.JobService, sort, dir string) []catalog.Manga {
	filter := catalog.SearchFilter{Sort: anilistSort(sort, dir)}
	if filter.Sort == "" {
		filter.Sort = "POPULARITY_DESC"
	}
	if usr := auth.FromContext(ctx); usr != nil {
		if options, err := svc.ContentTagOptions(ctx); err == nil && len(options) > 0 {
			genres, tags := splitByKind(usr.AllowedTags, options)
			if len(genres) == 0 || len(tags) == 0 {
				filter.GenreIn, filter.TagIn = genres, tags
			}
			filter.GenreNotIn, filter.TagNotIn = splitByKind(usr.BlockedTags, options)
		}
	}
	items, _, err := svc.SearchAniList(ctx, "", 18, filter)
	if err != nil {
		return nil
	}
	return items
}

// filterNSFWSources hides NSFW-only sources from users without adult access.
func filterNSFWSources(ctx context.Context, list []sources.Source) []sources.Source {
	u := auth.FromContext(ctx)
	if u != nil && u.AllowAdult {
		return list
	}
	out := make([]sources.Source, 0, len(list))
	for _, src := range list {
		if !src.NSFW {
			out = append(out, src)
		}
	}
	return out
}

// contentAllowed reports whether the acting user may see content with the
// given adult flag and tag/genre set. Guards apply to every user; the env
// admin's are simply unrestricted by default.
func contentAllowed(ctx context.Context, isAdult bool, tags []string) bool {
	u := auth.FromContext(ctx)
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

// mangaContentTags collects the tag/genre vocabulary a catalog entry carries.
func mangaContentTags(m catalog.Manga) []string {
	return append(append([]string{}, m.Tags...), m.Genres...)
}

// allowedManga drops catalog entries the acting user must not see.
func allowedManga(ctx context.Context, items []catalog.Manga) []catalog.Manga {
	out := make([]catalog.Manga, 0, len(items))
	for _, m := range items {
		if contentAllowed(ctx, m.IsAdult, mangaContentTags(m)) {
			out = append(out, m)
		}
	}
	return out
}

// titleAllowed reports whether the acting user may see the given title.
func titleAllowed(ctx context.Context, svc *service.JobService, titleID int64) bool {
	t, err := svc.GetTitle(ctx, titleID)
	return err == nil && contentAllowed(ctx, t.IsAdult, t.ContentTags)
}

// visibleTagOptions hides the acting user's blocked tags from filter pickers.
func visibleTagOptions(ctx context.Context, options []catalog.ContentTag) []catalog.ContentTag {
	u := auth.FromContext(ctx)
	if u == nil || len(u.BlockedTags) == 0 {
		return options
	}
	out := make([]catalog.ContentTag, 0, len(options))
	for _, o := range options {
		if !library.HasAnyTag([]string{o.Name}, u.BlockedTags) {
			out = append(out, o)
		}
	}
	return out
}

// theme returns the active UI theme and, for the custom theme, a CSS block of
// its color variables (values are hex-validated on save, so safe to embed).
func (u *webUI) theme(ctx context.Context) (string, template.CSS) {
	stored := u.svc.AllSettings(ctx) // global fallback (pre-multi-user values)
	for k, v := range u.svc.UserSettings(ctx, auth.UserID(ctx)) {
		stored[k] = v // personal appearance wins
	}
	theme := stored[service.SettingUITheme]
	if theme == "" {
		theme = service.SettingDefault(service.SettingUITheme)
	}
	if theme != "custom" {
		return theme, ""
	}
	var b strings.Builder
	b.WriteString(`[data-theme="custom"]{`)
	for _, token := range service.CustomColorTokens() {
		key := service.CustomColorKey(token)
		value, ok := stored[key]
		if !ok {
			value = service.SettingDefault(key)
		}
		if service.ValidateSetting(key, value) == nil {
			fmt.Fprintf(&b, "--color-%s:%s;", token, value)
		}
	}
	b.WriteString("}")
	return theme, template.CSS(b.String())
}

func (u *webUI) readerLayout(w http.ResponseWriter, r *http.Request, title string, data readerView) {
	theme, css := u.theme(r.Context())
	if err := u.tmpl.ExecuteTemplate(w, "reader_layout.html", pageData{Title: title, Theme: theme, ThemeCSS: css, Content: u.renderToHTML("reader", data)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *webUI) frag(w http.ResponseWriter, name string, data any) {
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (u *webUI) fail(w http.ResponseWriter, err error) {
	// htmx 2 does not swap 4xx bodies, and mutating controls use
	// hx-swap="none", so route the error toast to a global container with a
	// 200 status — otherwise failures are silently invisible.
	w.Header().Set("HX-Retarget", "#toast")
	w.Header().Set("HX-Reswap", "innerHTML")
	u.frag(w, "toast", toastView{Msg: err.Error()})
}

// --- pages ---

func (u *webUI) management(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if !user.Can(auth.PermStatsView) && !user.Can(auth.PermServicesView) && !user.Can(auth.PermSessionsView) && !user.Can(auth.PermJobsView) {
		writeError(w, http.StatusForbidden, "missing a management permission")
		return
	}
	u.page(w, r, "dashboard", "Management", dashData{User: user})
}

func (u *webUI) health(w http.ResponseWriter, r *http.Request) {
	u.frag(w, "servicesHealth", u.healthView(r.Context()))
}

func (u *webUI) healthView(ctx context.Context) healthView {
	return healthView{
		Services: u.svc.ServicesHealth(ctx),
		Interval: u.svc.Setting(ctx, service.SettingServicesHealthInterval, service.SettingDefault(service.SettingServicesHealthInterval)),
	}
}

const (
	libraryPerPage     = 20
	jobsPerPage        = 15
	jobChildrenShown   = 100
	collectionsPerPage = 24
)

func (u *webUI) libraryPage(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	view := libraryView{Table: u.buildLibraryTable(r.Context(), values)}
	if id, _ := strconv.ParseInt(values.Get("screen"), 10, 64); id > 0 {
		if sc, err := u.svc.GetScreen(r.Context(), id); err == nil {
			view.Screen = &sc
			view.Cfg = sc.Config
			view.IncludeTags = sc.Config.IncludeTags
			view.ExcludeTags = sc.Config.ExcludeTags
		}
	}
	view.OpenEditor = values.Get("new-screen") == "1" || (view.Screen != nil && values.Get("edit") == "1")
	view.ShowEditor = view.Screen != nil || view.OpenEditor
	view.TagOptions, _ = u.svc.StoredContentTags(r.Context())
	if len(view.TagOptions) == 0 && view.ShowEditor {
		view.TagOptions, _ = u.svc.ContentTagOptions(r.Context())
	}
	view.TagOptions = visibleTagOptions(r.Context(), view.TagOptions)
	view.Controls = libraryControlsFrom(screenDefaults(values, view.Screen))
	view.SourceOptions = u.librarySourceOptions(r.Context())
	if user := userFrom(r.Context()); user == nil || user.AllowAdult {
		view.AllowNSFW = true
	}
	title := "Library"
	if view.Screen != nil {
		title = view.Screen.Name
	}
	u.page(w, r, "library", title, view)
}

type collection struct {
	Name     string
	Members  []library.Title
	CustomID int64
	SmartKey string // set for a smart collection
}

// collectionCard is one collection rendered as a single library-style card:
// combined stats, a blended progress bar, and a cover collage of members.
type collectionCard struct {
	Name      string
	URL       string // link to the members subpage
	CustomID  int64  // >0 enables the rename/delete menu on the card
	SmartKey  string
	Count     int
	Chapters  int64
	Volumes   int64
	Pages     int64
	SizeBytes int64
	ReadPct   int64
	FullPct   int64
	Covers    []string // up to 3 member covers
	Extra     int      // remaining members beyond the shown covers
	Stacked   bool     // exactly 2 members
}

type collectionsView struct {
	Mode         string // "author" | "smart" | "custom"
	Cards        []collectionCard
	HasRelations bool
	Page         int
	PerPage      int
	Total        int
}

func (v collectionsView) TotalPages() int {
	if v.PerPage <= 0 {
		return 1
	}
	pages := (v.Total + v.PerPage - 1) / v.PerPage
	if pages < 1 {
		return 1
	}
	return pages
}

func (v collectionsView) HasPrev() bool { return v.Page > 1 }
func (v collectionsView) HasNext() bool { return v.Page < v.TotalPages() }
func (v collectionsView) Prev() int     { return v.Page - 1 }
func (v collectionsView) Next() int     { return v.Page + 1 }

func (v collectionsView) PageURL(page int) template.URL {
	q := url.Values{}
	if v.Mode == "smart" {
		q.Set("by", "relation")
	} else if v.Mode != "author" {
		q.Set("by", v.Mode)
	}
	q.Set("page", strconv.Itoa(page))
	return template.URL("/collections?" + q.Encode())
}

func (u *webUI) collectionsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	author, smart, custom, hasRelations := u.buildAllCollections(ctx)
	page, _, _ := tableParams(r.URL.Query(), collectionsPerPage)
	mode := r.URL.Query().Get("by")
	var cols []collection
	switch mode {
	case "relation":
		mode = "smart"
		cols = smart
	case "custom":
		cols = custom
	default:
		mode = "author"
		cols = author
	}
	pageCols, total := paginate(cols, page, collectionsPerPage)
	if pages := (total + collectionsPerPage - 1) / collectionsPerPage; pages > 0 && page > pages {
		page = pages
	}
	u.page(w, r, "collections", "Collections", collectionsView{
		Mode:         mode,
		Cards:        collectionCards(withMemberStats(ctx, u.svc, pageCols)),
		HasRelations: hasRelations,
		Page:         page,
		PerPage:      collectionsPerPage,
		Total:        total,
	})
}

// buildAllCollections gathers the library once and returns the three collection
// groupings plus whether any relation edges are stored.
func (u *webUI) buildAllCollections(ctx context.Context) (author, smart, custom []collection, hasRelations bool) {
	author, smart, custom, hasRelations, _ = buildCollections(ctx, u.svc)
	return author, smart, custom, hasRelations
}

func buildCollections(ctx context.Context, svc *service.JobService) (author, smart, custom []collection, hasRelations bool, err error) {
	q, ok := visibleTitles(ctx)
	if !ok {
		return nil, nil, nil, false, nil
	}
	titles, err := svc.TitleBasics(ctx, q)
	if err != nil {
		return nil, nil, nil, false, err
	}
	titleByID := make(map[int64]library.Title, len(titles))
	ids := make([]int64, 0, len(titles))
	for _, t := range titles {
		titleByID[t.ID] = t
		if t.CatalogMangaID != nil {
			ids = append(ids, *t.CatalogMangaID)
		}
	}
	mangas, _ := svc.MangaAuthors(ctx, ids)
	edges, _ := svc.CollectionEdges(ctx)
	pins, _ := svc.SmartPins(ctx)
	customs, _ := svc.CustomCollections(ctx)
	members, _ := svc.CollectionMembers(ctx)
	author = authorCollections(titles, mangas)
	smart = relationCollections(titles, mangas, edges, pins, titleByID)
	custom = customCollections(customs, members, titleByID)
	return author, smart, custom, len(edges) > 0, nil
}

// collectionMembersPage renders a single collection's titles using the same
// auto-layout library view. Custom collections (cid) load live membership and
// expose member management; other groupings use the ids snapshot in the query.
func (u *webUI) collectionMembersPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	values := r.URL.Query()
	name := values.Get("name")
	if name == "" {
		name = "Collection"
	}
	var collectionID int64
	if raw := values.Get("cid"); raw != "" {
		cid, _ := strconv.ParseInt(raw, 10, 64)
		customs, _ := u.svc.CustomCollections(ctx)
		owned := false
		for _, c := range customs {
			if c.ID == cid {
				owned, name = true, c.Name
				break
			}
		}
		if !owned {
			http.NotFound(w, r)
			return
		}
		collectionID = cid
		members, _ := u.svc.CollectionMembers(ctx)
		idStrs := make([]string, 0, len(members[cid]))
		for _, tid := range members[cid] {
			idStrs = append(idStrs, strconv.FormatInt(tid, 10))
		}
		ids := strings.Join(idStrs, ",")
		if ids == "" {
			ids = "0" // no title has id 0: an empty collection restricts to nothing
		}
		values.Set("ids", ids)
	}
	if values.Get("ids") == "" {
		http.NotFound(w, r)
		return
	}
	allowNSFW := userFrom(ctx) == nil || userFrom(ctx).AllowAdult
	view := libraryView{
		Table:              u.buildLibraryTable(ctx, values),
		Controls:           libraryControlsFrom(values),
		AllowNSFW:          allowNSFW,
		CollectionName:     name,
		CollectionIDs:      values.Get("ids"),
		CollectionID:       collectionID,
		CollectionSmartKey: values.Get("smart"),
		SourceOptions:      u.librarySourceOptions(ctx),
	}
	u.page(w, r, "library", name, view)
}

type collectionOption struct {
	Label    string
	Count    int
	CustomID int64
	SmartKey string
}

type addToCollectionView struct {
	TitleID int64
	Smart   []collectionOption
	Custom  []collectionOption
}

type collectionManageView struct {
	ID       int64
	SmartKey string
	Name     string
	Members  []library.Title
}

func collectionHasTitle(c collection, id int64) bool {
	for _, m := range c.Members {
		if m.ID == id {
			return true
		}
	}
	return false
}

// libCollectionsDialog lists collections a title is not yet in: smart ones it
// can be pinned to and custom ones it can be added to.
func (u *webUI) libCollectionsDialog(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	_, smart, custom, _ := u.buildAllCollections(r.Context())
	view := addToCollectionView{TitleID: titleID}
	for _, c := range smart {
		if collectionHasTitle(c, titleID) {
			continue
		}
		view.Smart = append(view.Smart, collectionOption{Label: c.Name, Count: len(c.Members), SmartKey: c.SmartKey})
	}
	for _, c := range custom {
		if collectionHasTitle(c, titleID) {
			continue
		}
		view.Custom = append(view.Custom, collectionOption{Label: c.Name, Count: len(c.Members), CustomID: c.CustomID})
	}
	u.frag(w, "addToCollection", view)
}

// libAddToCollection adds a title to a custom collection, pins it to a smart
// collection, or creates a new custom collection seeded with the title.
func (u *webUI) libAddToCollection(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	ctx := r.Context()
	if !titleAllowed(ctx, u.svc, titleID) {
		u.fail(w, fmt.Errorf("title not found"))
		return
	}
	_ = r.ParseForm()
	switch {
	case r.FormValue("cid") != "":
		var cid int64
		if cid, err = strconv.ParseInt(r.FormValue("cid"), 10, 64); err == nil {
			err = u.svc.AddToCollection(ctx, cid, titleID)
		}
	case r.FormValue("smart") != "":
		err = u.svc.PinToSmart(ctx, r.FormValue("smart"), titleID)
	case strings.TrimSpace(r.FormValue("name")) != "":
		var cid int64
		if cid, err = u.svc.CreateCollection(ctx, r.FormValue("name")); err == nil {
			err = u.svc.AddToCollection(ctx, cid, titleID)
		}
	default:
		err = fmt.Errorf("choose a collection")
	}
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "toast", toastView{OK: true, Msg: "Added to collection ✓"})
}

func (u *webUI) collectionCreate(w http.ResponseWriter, r *http.Request) {
	if _, err := u.svc.CreateCollection(r.Context(), r.FormValue("name")); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/collections?by=custom")
}

func (u *webUI) collectionRename(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.RenameCollection(r.Context(), id, r.FormValue("name")); err != nil {
		u.fail(w, err)
		return
	}
	target := r.Header.Get("HX-Current-URL")
	if target == "" {
		target = "/collections?by=custom"
	}
	w.Header().Set("HX-Redirect", target)
}

func (u *webUI) collectionDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.DeleteCollection(r.Context(), id); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/collections?by=custom")
}

func (u *webUI) collectionRemoveMember(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	tid, _ := strconv.ParseInt(r.FormValue("title"), 10, 64)
	if err := u.svc.RemoveFromCollection(r.Context(), id, tid); err != nil {
		u.fail(w, err)
		return
	}
	u.renderCollectionManage(w, r, id)
}

func (u *webUI) smartCollectionRemovePin(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	tid, _ := strconv.ParseInt(r.FormValue("title"), 10, 64)
	if key == "" || tid <= 0 {
		u.fail(w, fmt.Errorf("pinned title not found"))
		return
	}
	if err := u.svc.RemoveSmartPin(r.Context(), key, tid); err != nil {
		u.fail(w, err)
		return
	}
	u.renderSmartCollectionManage(w, r, key)
}

func (u *webUI) collectionManage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.renderCollectionManage(w, r, id)
}

func (u *webUI) smartCollectionManage(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		u.fail(w, fmt.Errorf("smart collection not found"))
		return
	}
	u.renderSmartCollectionManage(w, r, key)
}

func (u *webUI) renderCollectionManage(w http.ResponseWriter, r *http.Request, id int64) {
	ctx := r.Context()
	customs, _ := u.svc.CustomCollections(ctx)
	name, found := "", false
	for _, c := range customs {
		if c.ID == id {
			name, found = c.Name, true
			break
		}
	}
	if !found {
		u.fail(w, fmt.Errorf("collection not found"))
		return
	}
	members, _ := u.svc.CollectionMembers(ctx)
	mem := u.visibleMembers(ctx, members[id])
	u.frag(w, "collectionManage", collectionManageView{ID: id, Name: name, Members: mem})
}

func (u *webUI) renderSmartCollectionManage(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	pins, _ := u.svc.SmartPins(ctx)
	u.frag(w, "collectionManage", collectionManageView{SmartKey: key, Name: "Pinned titles", Members: u.visibleMembers(ctx, pins[key])})
}

func (u *webUI) visibleMembers(ctx context.Context, ids []int64) []library.Title {
	q, ok := visibleTitles(ctx)
	if !ok {
		return nil
	}
	q.IDs = append([]int64{}, ids...)
	mem, _ := u.svc.PageTitles(ctx, q)
	sortByTitle(mem)
	return mem
}

// withMemberStats swaps the basic member titles for fully aggregated ones.
func withMemberStats(ctx context.Context, svc *service.JobService, cols []collection) []collection {
	var ids []int64
	for _, c := range cols {
		for _, m := range c.Members {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return cols
	}
	full, _ := svc.PageTitles(ctx, library.TitleQuery{IDs: ids})
	byID := make(map[int64]library.Title, len(full))
	for _, t := range full {
		byID[t.ID] = t
	}
	out := make([]collection, len(cols))
	for i, c := range cols {
		members := make([]library.Title, len(c.Members))
		for j, m := range c.Members {
			if t, ok := byID[m.ID]; ok {
				m = t
			}
			members[j] = m
		}
		c.Members = members
		out[i] = c
	}
	return out
}

func collectionCards(cols []collection) []collectionCard {
	out := make([]collectionCard, 0, len(cols))
	for _, c := range cols {
		card := collectionCard{Name: c.Name, Count: len(c.Members), CustomID: c.CustomID, SmartKey: c.SmartKey}
		var total, read int64
		ids := make([]string, 0, len(c.Members))
		for _, m := range c.Members {
			card.Chapters += m.DiscoveredCount
			card.Volumes += m.VolumeCount
			card.Pages += m.Pages + m.VolumePages
			card.SizeBytes += m.SizeBytes + m.VolumeBytes
			total += m.DiscoveredCount + m.VolumeCount
			read += m.ReadCount + m.VolumeReadCount
			ids = append(ids, strconv.FormatInt(m.ID, 10))
		}
		q := url.Values{"name": {c.Name}, "ids": {strings.Join(ids, ",")}}
		if c.CustomID > 0 {
			q.Set("cid", strconv.FormatInt(c.CustomID, 10))
		}
		if c.SmartKey != "" {
			q.Set("smart", c.SmartKey)
		}
		card.URL = "/collections/view?" + q.Encode()
		card.ReadPct = percent(read, total)
		if total > 0 {
			card.FullPct = 100 // blue fills the whole bar: the combined total
		}
		n := len(c.Members)
		if n == 2 {
			card.Stacked = true
		} else if n > 4 {
			n = 3
			card.Extra = len(c.Members) - 3
		}
		for _, m := range c.Members[:n] {
			card.Covers = append(card.Covers, m.CoverImage)
		}
		out = append(out, card)
	}
	return out
}

// customCollections turns stored custom collections into renderable collections,
// resolving member title ids against the (content-filtered) library.
func customCollections(cols []library.Collection, members map[int64][]int64, titleByID map[int64]library.Title) []collection {
	out := make([]collection, 0, len(cols))
	for _, c := range cols {
		var mem []library.Title
		for _, tid := range members[c.ID] {
			if t, ok := titleByID[tid]; ok {
				mem = append(mem, t)
			}
		}
		sortByTitle(mem)
		out = append(out, collection{Name: c.Name, Members: mem, CustomID: c.ID})
	}
	return out
}

func authorCollections(titles []library.Title, mangas map[int64]catalog.Manga) []collection {
	groups := map[string]*collection{}
	seen := map[string]bool{} // "authorKey\x00titleID" — a manga listing an author twice
	var order []string
	for _, t := range titles {
		if t.CatalogMangaID == nil {
			continue
		}
		for _, author := range mangas[*t.CatalogMangaID].Authors {
			key := strings.ToLower(strings.TrimSpace(author))
			if key == "" {
				continue
			}
			dedupe := key + "\x00" + strconv.FormatInt(t.ID, 10)
			if seen[dedupe] {
				continue
			}
			seen[dedupe] = true
			disp := strings.TrimSpace(author)
			g := groups[key]
			if g == nil {
				g = &collection{Name: disp}
				groups[key] = g
				order = append(order, key)
			} else if disp < g.Name {
				g.Name = disp
			}
			g.Members = append(g.Members, t)
		}
	}

	bySig := map[string]*collection{}
	sigAuthors := map[string][]string{}
	var sigOrder []string
	for _, key := range order {
		g := groups[key]
		if len(g.Members) < 2 {
			continue
		}
		sortByTitle(g.Members)
		sig := memberSignature(g.Members)
		if _, ok := bySig[sig]; !ok {
			bySig[sig] = &collection{Members: g.Members}
			sigOrder = append(sigOrder, sig)
		}
		sigAuthors[sig] = append(sigAuthors[sig], g.Name)
	}
	var out []collection
	for _, sig := range sigOrder {
		authors := sigAuthors[sig]
		sort.SliceStable(authors, func(i, j int) bool {
			return strings.ToLower(authors[i]) < strings.ToLower(authors[j])
		})
		c := bySig[sig]
		c.Name = strings.Join(authors, ", ")
		out = append(out, *c)
	}
	sortByName(out)
	return out
}

func memberSignature(members []library.Title) string {
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(strconv.FormatInt(id, 10))
		b.WriteByte(',')
	}
	return b.String()
}

func relationCollections(titles []library.Title, mangas map[int64]catalog.Manga, edges [][2]string, pins map[string][]int64, titleByID map[int64]library.Title) []collection {
	byProvider := map[string][]library.Title{}
	var pids []string
	for _, t := range titles {
		if t.CatalogMangaID == nil {
			continue
		}
		if pid := mangas[*t.CatalogMangaID].ProviderID; pid != "" {
			if _, ok := byProvider[pid]; !ok {
				pids = append(pids, pid)
			}
			byProvider[pid] = append(byProvider[pid], t)
		}
	}
	adj := map[string][]string{}
	for _, e := range edges {
		if _, ok := byProvider[e[0]]; !ok {
			continue
		}
		if _, ok := byProvider[e[1]]; !ok {
			continue
		}
		adj[e[0]] = append(adj[e[0]], e[1])
		adj[e[1]] = append(adj[e[1]], e[0])
	}
	seen := map[string]bool{}
	var out []collection
	for _, pid := range pids {
		if seen[pid] {
			continue
		}
		var members []library.Title
		var component []string
		stack := []string{pid}
		seen[pid] = true
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			component = append(component, cur)
			members = append(members, byProvider[cur]...)
			for _, next := range adj[cur] {
				if !seen[next] {
					seen[next] = true
					stack = append(stack, next)
				}
			}
		}
		if len(members) < 2 {
			continue
		}
		key := smartKeyFor(component)
		have := make(map[int64]bool, len(members))
		for _, m := range members {
			have[m.ID] = true
		}
		for _, tid := range pins[key] {
			if !have[tid] {
				if t, ok := titleByID[tid]; ok {
					members = append(members, t)
					have[tid] = true
				}
			}
		}
		sortByTitle(members)
		out = append(out, collection{Name: collectionName(members), Members: members, SmartKey: key})
	}
	sortByName(out)
	return out
}

// smartKeyFor is a stable identity for a smart component: its smallest provider
// id (numeric where possible), used to persist pinned members.
func smartKeyFor(pids []string) string {
	best := ""
	var bestN int64
	haveN := false
	for _, p := range pids {
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			if !haveN || n < bestN {
				bestN, best, haveN = n, p, true
			}
		}
	}
	if !haveN {
		for _, p := range pids {
			if best == "" || p < best {
				best = p
			}
		}
	}
	return best
}

// collectionName names a franchise after its shortest member title, which is
// usually the root work (e.g. "The Ghost in the Shell").
func collectionName(members []library.Title) string {
	name := members[0].DisplayTitle
	for _, m := range members[1:] {
		if len(m.DisplayTitle) < len(name) {
			name = m.DisplayTitle
		}
	}
	return name
}

func sortByTitle(ts []library.Title) {
	sort.SliceStable(ts, func(i, j int) bool {
		return strings.ToLower(ts[i].DisplayTitle) < strings.ToLower(ts[j].DisplayTitle)
	})
}

func sortByName(cs []collection) {
	sort.SliceStable(cs, func(i, j int) bool {
		return strings.ToLower(cs[i].Name) < strings.ToLower(cs[j].Name)
	})
}

func screenFromForm(r *http.Request) library.Screen {
	_ = r.ParseForm()
	return library.Screen{
		Name: r.FormValue("name"),
		Config: library.ScreenConfig{
			IncludeTags: tagList(r.Form["include_tags"]),
			ExcludeTags: tagList(r.Form["exclude_tags"]),
			Adult:       r.FormValue("adult"),
			Monitor:     r.FormValue("monitor"),
			Fav:         r.FormValue("fav"),
			Progress:    r.FormValue("progress"),
			Content:     r.FormValue("content"),
			Sort:        r.FormValue("sort"),
			Dir:         r.FormValue("dir"),
			View:        r.FormValue("view"),
		},
	}
}

func (u *webUI) screenSave(w http.ResponseWriter, r *http.Request) {
	sc := screenFromForm(r)
	if raw := r.PathValue("id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			u.fail(w, err)
			return
		}
		sc.ID = id
	}
	id, err := u.svc.SaveScreen(r.Context(), sc)
	if err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library?screen=%d", id))
}

func (u *webUI) screenReorder(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	var ids []int64
	for _, raw := range r.Form["order"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if err := u.svc.ReorderScreens(r.Context(), ids); err != nil {
		u.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (u *webUI) screenDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.DeleteScreen(r.Context(), id); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/library")
}

func (u *webUI) libraryTable(w http.ResponseWriter, r *http.Request) {
	u.frag(w, "libraryResults", u.buildLibraryTable(r.Context(), r.URL.Query()))
}

// libraryResults renders the library as a table, a card grid, or both
// (responsive auto mode).
type libraryResults struct {
	Screen int64
	tableData
	Cards     []library.Title
	Fulls     []libraryFullRow // detailed rows for the full view
	CanManage bool
	View      string
}

type libraryFullRow struct {
	Title library.Title
	Manga catalog.Manga
}

func (u *webUI) readerPage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	volumesMode := r.URL.Query().Get("mode") == "volumes"
	var progress library.TitleReadProgress
	var err2 error
	if volumesMode {
		progress, err2 = u.svc.VolumesReaderProgress(r.Context(), id)
	} else {
		progress, err2 = u.svc.ReaderProgress(r.Context(), id)
	}
	if err2 != nil {
		http.Error(w, err2.Error(), http.StatusBadRequest)
		return
	}
	if !contentAllowed(r.Context(), progress.Title.IsAdult, progress.Title.ContentTags) {
		http.Error(w, "This content is not available for this account", http.StatusForbidden)
		return
	}
	presence.SetTitle(r.Context(), auth.UserID(r.Context()), progress.ID, progress.DisplayTitle)
	currentID, _ := strconv.ParseInt(r.URL.Query().Get("chapter"), 10, 64)
	manifest, prevID, nextID := readerManifestWindowMode(progress, currentID, volumesMode)
	data := readerView{Title: progress.Title, Manifest: manifest, PagePosition: initialReaderPosition(manifest)}
	if len(data.Manifest.Chapters) == 0 {
		data.Empty = "No downloaded chapters are available to read yet."
	}
	mode := ""
	if volumesMode {
		mode = "&mode=volumes"
	}
	if prevID > 0 {
		data.PrevURL = fmt.Sprintf("/reader/%d?chapter=%d%s", progress.ID, prevID, mode)
	}
	if nextID > 0 {
		data.NextURL = fmt.Sprintf("/reader/%d?chapter=%d%s", progress.ID, nextID, mode)
	}
	if raw, err := json.Marshal(data.Manifest); err == nil {
		data.ManifestJSON = template.JS(raw)
	}
	u.readerLayout(w, r, progress.DisplayTitle, data)
}

func initialReaderPosition(manifest readerManifestResponse) string {
	if manifest.ResumePage <= 0 {
		return ""
	}
	for _, chapter := range manifest.Chapters {
		if chapter.ID == manifest.ResumeChapterID && chapter.PageCount > 0 {
			return fmt.Sprintf("%d/%d", manifest.ResumePage, chapter.PageCount)
		}
	}
	return strconv.Itoa(manifest.ResumePage)
}

func (u *webUI) buildLibraryTable(ctx context.Context, values url.Values) libraryResults {
	page, _, _ := tableParams(values, libraryPerPage)
	var screen *library.Screen
	var screenID int64
	if id, _ := strconv.ParseInt(values.Get("screen"), 10, 64); id > 0 {
		if sc, err := u.svc.GetScreen(ctx, id); err == nil {
			screen = &sc
			screenID = id
		}
	}
	controls := libraryControlsFrom(screenDefaults(values, screen))
	base, visible := visibleTitles(ctx)
	if ids := values.Get("ids"); ids != "" {
		base.IDs = parseIDs(ids)
	}
	q := controls.apply(base)
	if screen != nil {
		q = applyScreen(q, screen.Config)
	}
	var pageTitles []library.Title
	var total, allCount int
	if visible {
		total, _ = u.svc.CountTitles(ctx, q)
		if total > 0 {
			q.Limit = libraryPerPage
			q.Offset = min((page-1)*libraryPerPage, ((total-1)/libraryPerPage)*libraryPerPage)
			pageTitles, _ = u.svc.PageTitles(ctx, q)
		}
		allCount = total
		if total == 0 {
			allCount, _ = u.svc.CountTitles(ctx, base)
		}
	}
	pageIDs := make([]int64, len(pageTitles))
	for i, tl := range pageTitles {
		pageIDs[i] = tl.ID
	}
	byTitle := map[int64][]jobs.Job{}
	for _, j := range u.jobs(ctx, pageIDs...) {
		var p service.JobPayload
		if json.Unmarshal([]byte(j.Payload), &p) == nil {
			byTitle[p.TitleID] = append(byTitle[p.TitleID], j)
		}
	}
	empty := "Nothing in your library yet — add manga from Search or Import a collection."
	if allCount > 0 {
		empty = "No manga match the current search or filters."
	}

	t := libraryResults{Screen: screenID, View: controls.View, CanManage: auth.FromContext(ctx).Can(auth.PermLibraryManage)}
	columns := []tableColumn{
		{Label: ""},
		{Label: "Title"},
		{Label: "Chapters"},
	}
	if t.CanManage {
		columns = append([]tableColumn{{Label: ""}}, columns...)
	}
	t.tableData = tableData{
		ID: "library-table", BaseURL: "/ui/library/table",
		Page: page, PerPage: libraryPerPage, Total: total, Sort: controls.Sort, Dir: controls.Dir,
		Params: libraryTableParams(values),
		Empty:  empty, Columns: columns,
	}
	t.Cards = pageTitles
	catalogIDs := make([]int64, 0, len(pageTitles))
	for _, tl := range pageTitles {
		if tl.CatalogMangaID != nil {
			catalogIDs = append(catalogIDs, *tl.CatalogMangaID)
		}
	}
	mangas, _ := u.svc.MangaByIDs(ctx, catalogIDs) // one query for all page rows
	for _, tl := range pageTitles {
		running, label, queued, failed, msg := titleActivityFrom(byTitle[tl.ID], tl)
		_ = queued
		if len(running) > 0 {
			t.Poll = true
		}
		view := activityView{Title: tl, Running: running, ActiveLabel: label, Failed: failed, Error: msg, User: auth.FromContext(ctx)}
		full := libraryFullRow{Title: tl}
		var detail template.HTML
		if tl.CatalogMangaID != nil {
			if m, ok := mangas[*tl.CatalogMangaID]; ok {
				detail = u.renderToHTML("mangaDetail", m)
				full.Manga = m
			}
		}
		t.Fulls = append(t.Fulls, full)
		rowClass := ""
		if tl.IsAdult {
			rowClass = "outline outline-1 -outline-offset-1 outline-error"
		}
		cells := []template.HTML{
			u.renderToHTML("cellCover", tl),
			u.renderToHTML("cellTitle", view),
			u.renderToHTML("progressBar", tl),
		}
		if t.CanManage {
			cells = append([]template.HTML{u.renderToHTML("bulkTitleCheckbox", tl)}, cells...)
		}
		t.Rows = append(t.Rows, tableRow{
			ID:     strconv.FormatInt(tl.ID, 10),
			Class:  rowClass,
			Cells:  cells,
			Detail: detail,
		})
	}
	return t
}

func (u *webUI) librarySourceOptions(ctx context.Context) []sources.Source {
	srcs, _ := u.svc.ListSources(ctx)
	srcs = filterNSFWSources(ctx, srcs)
	counts, _ := u.svc.SourcesInUse(ctx)
	out := srcs[:0]
	for _, s := range srcs {
		if counts[s.ID] > 0 {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// selectedSources trims the source tokens, dropping blanks and a stray "all".
func selectedSources(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" && s != "all" {
			out = append(out, s)
		}
	}
	return out
}

func libraryControlsFrom(values url.Values) libraryControls {
	c := libraryControls{
		Q:           strings.TrimSpace(values.Get("q")),
		Monitor:     values.Get("monitor"),
		Fav:         values.Get("fav"),
		Content:     values.Get("content"),
		NSFW:        values.Get("nsfw"),
		Sources:     selectedSources(values["source"]),
		Progress:    values.Get("progress"),
		Sort:        values.Get("sort"),
		Dir:         values.Get("dir"),
		View:        values.Get("view"),
		IncludeTags: tagList(values["include_tags"]),
		ExcludeTags: tagList(values["exclude_tags"]),
	}
	if c.View != "table" && c.View != "cards" && c.View != "full" {
		c.View = "auto"
	}
	if c.Monitor == "" {
		c.Monitor = "all"
	}
	if c.Fav != "only" {
		c.Fav = "all"
	}
	switch c.Content {
	case "chapters", "no-chapters", "volumes", "no-volumes":
	default:
		c.Content = "all"
	}
	if c.Progress == "" {
		c.Progress = "all"
	}
	if c.Sort == "" {
		c.Sort = "added"
		if c.Dir == "" {
			c.Dir = "desc" // newest additions first by default
		}
	}
	if c.Dir != "desc" {
		c.Dir = "asc"
	}
	return c
}

// screenDefaults fills unset filter controls from a screen's saved defaults.
func screenDefaults(values url.Values, screen *library.Screen) url.Values {
	if screen == nil {
		return values
	}
	out := url.Values{}
	for k, v := range values {
		out[k] = v
	}
	for key, def := range map[string]string{
		"monitor": screen.Config.Monitor, "fav": screen.Config.Fav,
		"source": screen.Config.Source, "progress": screen.Config.Progress,
		"content": screen.Config.Content, "sort": screen.Config.Sort,
		"dir": screen.Config.Dir, "view": screen.Config.View,
	} {
		if def != "" && out.Get(key) == "" {
			out.Set(key, def)
		}
	}
	return out
}

func libraryTableParams(values url.Values) url.Values {
	out := url.Values{}
	for _, key := range []string{"q", "monitor", "fav", "nsfw", "progress", "content", "sort", "dir", "view", "screen", "ids"} {
		if value := strings.TrimSpace(values.Get(key)); value != "" {
			out.Set(key, value)
		}
	}
	for _, key := range []string{"source", "include_tags", "exclude_tags"} {
		for _, value := range values[key] {
			if value = strings.TrimSpace(value); value != "" {
				out.Add(key, value)
			}
		}
	}
	return out
}

// tagList flattens repeated form values, splitting comma-list fallbacks.
func tagList(raw []string) []string {
	var out []string
	for _, v := range raw {
		out = append(out, splitCommaList(v)...)
	}
	return out
}

type tagChipView struct {
	Name    string
	Tooltip string
	IsAdult bool
	State   string // "include" | "exclude" | ""
}

type tagGroupView struct {
	Title string
	Tags  []tagChipView
}

// tagCategoryLabel turns an AniList category like "Theme-Fantasy" into a
// readable group heading; empty categories fall under "Other".
func tagCategoryLabel(category string) string {
	category = strings.TrimSpace(category)
	if category == "" {
		return "Other"
	}
	return strings.Replace(category, "-", " – ", 1)
}

// tagFilterView groups the tag vocabulary for the tri-state include/exclude
// chip filter: genres first, then one group per AniList tag category, marking
// each tag's state and keeping unknown stored values as a Custom group.
func tagFilterView(options []catalog.ContentTag, include, exclude []string, label string) map[string]any {
	inc := map[string]bool{}
	exc := map[string]bool{}
	state := func(name string) string {
		if inc[name] {
			return "include"
		}
		if exc[name] {
			return "exclude"
		}
		return ""
	}
	for _, v := range include {
		if v = strings.TrimSpace(v); v != "" {
			inc[v] = true
		}
	}
	for _, v := range exclude {
		if v = strings.TrimSpace(v); v != "" {
			exc[v] = true
		}
	}
	var genres []tagChipView
	var groups []tagGroupView
	byLabel := map[string]int{} // category label -> index in groups
	known := map[string]bool{}
	for _, o := range options {
		known[o.Name] = true
		chip := tagChipView{Name: o.Name, Tooltip: o.Description, IsAdult: o.IsAdult, State: state(o.Name)}
		if o.Kind == "genre" {
			genres = append(genres, chip)
			continue
		}
		title := tagCategoryLabel(o.Category)
		i, ok := byLabel[title]
		if !ok {
			i = len(groups)
			byLabel[title] = i
			groups = append(groups, tagGroupView{Title: title})
		}
		groups[i].Tags = append(groups[i].Tags, chip)
	}
	var extras []tagChipView
	for _, v := range append(append([]string{}, include...), exclude...) {
		if v = strings.TrimSpace(v); v != "" && !known[v] {
			known[v] = true
			extras = append(extras, tagChipView{Name: v, State: state(v)})
		}
	}
	var ordered []tagGroupView
	if len(genres) > 0 {
		ordered = append(ordered, tagGroupView{Title: "Genres", Tags: genres})
	}
	ordered = append(ordered, groups...)
	if len(extras) > 0 {
		ordered = append(ordered, tagGroupView{Title: "Custom", Tags: extras})
	}
	return map[string]any{
		"HasOptions":   len(options) > 0,
		"Label":        label,
		"Groups":       ordered,
		"Include":      include,
		"Exclude":      exclude,
		"IncludeCount": len(inc),
		"ExcludeCount": len(exc),
	}
}

func visibleTitles(ctx context.Context) (library.TitleQuery, bool) {
	u := auth.FromContext(ctx)
	if u == nil {
		return library.TitleQuery{}, false
	}
	q := library.TitleQuery{NoAdult: !u.AllowAdult, Exclude: u.BlockedTags}
	if len(u.AllowedTags) > 0 {
		q.IncludeAny = [][]string{u.AllowedTags}
	}
	return q, true
}

func (c libraryControls) apply(q library.TitleQuery) library.TitleQuery {
	q.Search = c.Q
	if len(c.IncludeTags) > 0 {
		q.IncludeAny = append(append([][]string{}, q.IncludeAny...), c.IncludeTags)
	}
	q.Exclude = append(append([]string{}, q.Exclude...), c.ExcludeTags...)
	q.Monitored = c.Monitor
	q.FavouriteOnly = c.Fav == "only"
	q.AdultOnly = q.AdultOnly || c.NSFW == "only"
	q.Sources = c.Sources
	q.Progress = c.Progress
	q.Content = c.Content
	q.Sort, q.Dir = c.Sort, c.Dir
	return q
}

func applyScreen(q library.TitleQuery, sc library.ScreenConfig) library.TitleQuery {
	switch sc.Adult {
	case "only":
		q.AdultOnly = true
	case "exclude":
		q.NoAdult = true
	}
	if len(sc.IncludeTags) > 0 {
		q.IncludeAny = append(append([][]string{}, q.IncludeAny...), sc.IncludeTags)
	}
	q.Exclude = append(append([]string{}, q.Exclude...), sc.ExcludeTags...)
	return q
}

func parseIDs(csv string) []int64 {
	ids := []int64{}
	for _, s := range strings.Split(csv, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (u *webUI) jobCancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		u.fail(w, fmt.Errorf("invalid job id"))
		return
	}
	if err := u.svc.CancelJob(r.Context(), id); err != nil {
		u.fail(w, err)
		return
	}
	u.jobsTable(w, r)
}

// globalJobTypes are the sweep jobs a user can start by hand.
var globalJobTypes = []string{
	jobs.TypeRefreshTitle,
	jobs.TypeScanDownloads,
	jobs.TypeDownloadMissing,
	jobs.TypeDetectCrops,
	jobs.TypeCatalogRefresh,
	jobs.TypeSyncAniList,
	jobs.TypeBackupUserData,
}

func (u *webUI) jobStart(w http.ResponseWriter, r *http.Request) {
	typ := r.FormValue("type")
	if !slices.Contains(globalJobTypes, typ) {
		u.fail(w, fmt.Errorf("unknown job type"))
		return
	}
	if _, err := u.svc.Enqueue(r.Context(), typ, 0, time.Now()); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	u.jobsTable(w, r)
}

func (u *webUI) jobsTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, key, dir := tableParams(r.URL.Query(), jobsPerPage)
	rows, total, _ := u.svc.TopLevelJobs(ctx, key, dir, (page-1)*jobsPerPage, jobsPerPage)
	ids := make([]int64, len(rows))
	for i, j := range rows {
		ids[i] = j.ID
	}
	children, _ := u.svc.JobChildren(ctx, ids, jobChildrenShown)
	active, _ := u.svc.FindJobs(ctx, jobs.Filter{Statuses: []string{"queued", "running"}, Limit: 1})

	t := tableData{
		ID: "jobs-table", BaseURL: "/ui/jobs/table",
		Page: page, PerPage: jobsPerPage, Total: total, Sort: key, Dir: dir,
		Poll: len(active) > 0, Empty: "No jobs yet.",
		Columns: []tableColumn{
			{Label: ""},
			{Label: ""},
		},
	}
	for _, j := range rows {
		var kids []jobs.Job
		activeKids, deadKids, cancelledKids, kidCount := 0, 0, 0, 0
		if c := children[j.ID]; c != nil {
			kids = c.Newest
			activeKids = c.Counts["queued"] + c.Counts["running"] + c.Counts["failed"]
			deadKids, cancelledKids, kidCount = c.Counts["dead"], c.Counts["cancelled"], c.Total
		}
		selfActive, _ := jobState(j.Status)
		// A group is only "done" once every child finished successfully; the
		// status icon rolls up the children's worst outcome.
		display := j
		if kidCount > 0 {
			switch {
			case selfActive || activeKids > 0:
				display.Status = "running"
			case deadKids > 0:
				display.Status = "dead"
			case cancelledKids > 0:
				display.Status = "cancelled"
			}
		}
		detail := u.renderToHTML("jobDetail", j)
		if kidCount > 0 {
			detail = u.renderToHTML("jobChildren", kids)
		}
		t.Rows = append(t.Rows, tableRow{
			ID: strconv.FormatInt(j.ID, 10),
			Cells: []template.HTML{
				u.renderToHTML("jobCell", jobGroupView{Job: j, Children: kidCount, ActiveChildren: activeKids}),
				u.renderToHTML("jobActions", jobActionsView{Job: display, CanCancel: selfActive || activeKids > 0}),
			},
			Detail: detail,
		})
	}
	u.frag(w, "table", t)
}

func (u *webUI) titlePage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		http.Error(w, "This content is not available for this account", http.StatusForbidden)
		return
	}
	view := u.titleActivity(r.Context(), id)
	view.Title = title
	view.User = userFrom(r.Context())
	if title.CatalogMangaID != nil {
		view.AniList = u.svc.AniListConnectionFor(r.Context(), auth.UserID(r.Context()))
	}
	view.Content = u.buildTitleContent(r, title, r.URL.Query().Get("tab"))
	view.RefreshEvery = u.svc.Setting(r.Context(), service.SettingServeRefreshEvery, service.SettingDefault(service.SettingServeRefreshEvery))
	linked := u.linkedSourceIDs(r.Context(), id)
	allSources, _ := u.svc.ListSources(r.Context()) // one fetch for every source-derived section
	allSources = filterNSFWSources(r.Context(), allSources)
	view.LinkedSources = u.linkedSourceViews(r.Context(), title)
	view.Sources = u.sourceView(r.Context(), title, linked, allSources)
	view.SingleSources = filterSources(singleMangaSources(allSources), linked)
	view.LinkSources = filterSources(searchableSources(allSources), linked)
	if title.CatalogMangaID != nil {
		view.Manga, _ = u.svc.GetManga(r.Context(), *title.CatalogMangaID)
	}
	u.page(w, r, "title", title.DisplayTitle, view)
}

// singleMangaSources returns enabled sources flagged as single-manga.
func singleMangaSources(all []sources.Source) []sources.Source {
	var out []sources.Source
	for _, s := range all {
		if s.SingleManga && s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// searchableSources returns enabled multi-manga sources, for specifying a page.
func searchableSources(all []sources.Source) []sources.Source {
	var out []sources.Source
	for _, s := range all {
		if s.Enabled && !s.SingleManga {
			out = append(out, s)
		}
	}
	return out
}

const chaptersPerPage = 25

// titleContentView drives the tabbed Chapters/Volumes section.
type titleContentView struct {
	Title           library.Title
	Tab             string // "chapters" or "volumes"
	ReadLabel       string
	VolumeReadLabel string
	ChaptersTable   chaptersView
	ChapterCount    int64
	Volumes         []volumeRowView
	VolumeCount     int
	Attaching       bool
	CanManage       bool
}

func (u *webUI) buildTitleContent(r *http.Request, title library.Title, tab string) titleContentView {
	ctx := r.Context()
	vols, _ := u.svc.Volumes(ctx, title.ID)
	running, _, queued, _, _ := titleActivityFrom(u.jobs(ctx, title.ID), title)
	attaching := running[jobs.TypeAttachVolumes] || slices.Contains(queued, "attaching volumes")
	if tab == "" && title.DiscoveredCount == 0 && (len(vols) > 0 || attaching) {
		tab = "volumes"
	}
	if tab != "volumes" || (len(vols) == 0 && !attaching) {
		tab = "chapters"
	}
	view := titleContentView{
		Title:        title,
		Tab:          tab,
		ChapterCount: title.DiscoveredCount,
		Volumes:      volumeRows(vols),
		VolumeCount:  len(vols),
		Attaching:    attaching,
		CanManage:    auth.FromContext(ctx).Can(auth.PermLibraryManage),
	}
	view.ReadLabel = "Read"
	if progress, err := u.svc.ReaderProgress(ctx, title.ID); err == nil && progress.NextChapterID != 0 && progress.ReadPages > 0 {
		view.ReadLabel = "Continue reading"
	}
	view.VolumeReadLabel = "Read"
	for _, v := range vols {
		if v.ReadPages > 0 || v.Read {
			view.VolumeReadLabel = "Continue reading"
			break
		}
	}
	if tab == "chapters" {
		view.ChaptersTable = u.buildChaptersTable(ctx, title, r.URL.Query())
		view.ChapterCount = int64(view.ChaptersTable.Total)
	}
	return view
}

func (u *webUI) titleContentFrag(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.writeTitleContent(w, r, id, r.URL.Query().Get("tab"))
}

func (u *webUI) writeTitleContent(w http.ResponseWriter, r *http.Request, titleID int64, tab string) {
	title, err := u.svc.GetTitle(r.Context(), titleID)
	if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		http.NotFound(w, r)
		return
	}
	u.frag(w, "titleContent", u.buildTitleContent(r, title, tab))
}

// titleProgress re-renders the header progress bar; it polls while a job for
// the title is active so downloads update the page live.
type favView struct {
	ID        int64
	Favourite bool
}

func (u *webUI) libLanguageMode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	if err := u.svc.SetLanguageMode(r.Context(), id, r.FormValue("mode")); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	w.Header().Set("HX-Refresh", "true")
}

func (u *webUI) libFavourite(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	fav, err := u.svc.ToggleFavourite(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "favToggle", favView{ID: id, Favourite: fav})
}

func (u *webUI) titleProgress(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		http.NotFound(w, r)
		return
	}
	running, _, queued, _, _ := titleActivityFrom(u.jobs(r.Context(), title.ID), title)
	u.frag(w, "titleProgress", activityView{Title: title, Running: running, Queued: queued})
}

func (u *webUI) chaptersTable(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, id)
}

// chapterDownload streams one chapter's CBZ file to the device.
func (u *webUI) chapterDownload(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chapterID, err := parseInt64Path(r, "chapterID")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st, err := u.svc.ChapterReadStatus(r.Context(), chapterID)
	if err != nil || st.TitleID != titleID || !st.Downloaded || st.OutputFile == "" ||
		!titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(st.OutputFile)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot read chapter", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.comicbook+zip")
	w.Header().Set("Content-Disposition", contentDisposition(cbzFileName(st.Label)))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// chaptersDownload streams a ZIP of the downloaded chapters in a from–to range.
func (u *webUI) chaptersDownload(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil || !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	from, ferr := strconv.Atoi(r.URL.Query().Get("from"))
	to, terr := strconv.Atoi(r.URL.Query().Get("to"))
	if ferr != nil || terr != nil {
		http.Error(w, "from and to are required", http.StatusBadRequest)
		return
	}
	if from > to {
		from, to = to, from
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chs, _ := u.svc.TitleChapters(r.Context(), id)
	var selected []library.ChapterStatus
	for _, c := range chs {
		if c.Downloaded && c.OutputFile != "" && c.NumberMain >= from && c.NumberMain <= to {
			selected = append(selected, c)
		}
	}
	if len(selected) == 0 {
		http.Error(w, "no downloaded chapters in range", http.StatusNotFound)
		return
	}

	tmp, err := os.CreateTemp("", "kaodoku-download-*.zip")
	if err != nil {
		http.Error(w, "cannot prepare download", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	zw := zip.NewWriter(tmp)
	if name, cover, ok := fetchCover(r.Context(), title.CoverImage); ok {
		if dst, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store}); err == nil {
			_, _ = dst.Write(cover)
		}
	}
	for _, c := range selected {
		src, err := os.Open(c.OutputFile)
		if err != nil {
			continue
		}
		dst, err := zw.CreateHeader(&zip.FileHeader{Name: cbzFileName(c.Label), Method: zip.Store})
		if err == nil {
			_, err = io.Copy(dst, src)
		}
		_ = src.Close()
		if err != nil {
			http.Error(w, "cannot build download", http.StatusInternalServerError)
			return
		}
	}
	if err := zw.Close(); err != nil {
		http.Error(w, "cannot build download", http.StatusInternalServerError)
		return
	}
	info, err := tmp.Stat()
	if err != nil {
		http.Error(w, "cannot build download", http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("%s-%d-%d.zip", safeFileName(title.DisplayTitle, "title"), from, to)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	http.ServeContent(w, r, "", info.ModTime(), tmp)
}

// fetchCover downloads the manga cover image so it can ride along in the ZIP.
// Failure is non-fatal — the archive is still worth serving without it.
func fetchCover(ctx context.Context, coverURL string) (name string, data []byte, ok bool) {
	if strings.TrimSpace(coverURL) == "" {
		return "", nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL, nil)
	if err != nil {
		return "", nil, false
	}
	client, err := util.NewHTTPClient(util.HTTPClientOptions{Timeout: 20 * time.Second, BlockPrivateNetworks: true})
	if err != nil {
		return "", nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, false
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil || len(data) == 0 {
		return "", nil, false
	}
	ext := strings.ToLower(path.Ext(coverURL))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif":
	default:
		ext = ".jpg"
	}
	return "cover" + ext, data, true
}

var unsafeFileChars = regexp.MustCompile(`[^\w.\- ]+`)

func safeFileName(name, fallback string) string {
	name = strings.TrimSpace(unsafeFileChars.ReplaceAllString(name, "_"))
	if name == "" {
		return fallback
	}
	return name
}

func cbzFileName(label string) string {
	name := safeFileName(label, "chapter")
	if !strings.HasSuffix(strings.ToLower(name), ".cbz") && !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name += ".cbz"
	}
	return name
}

func contentDisposition(name string) string {
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", name, url.PathEscape(name))
}

func (u *webUI) chapterRead(read bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		titleID, err := pathID(r)
		if err != nil {
			u.fail(w, err)
			return
		}
		chapterID, err := parseInt64Path(r, "chapterID")
		if err != nil {
			u.fail(w, err)
			return
		}
		status, err := u.svc.ChapterReadStatus(r.Context(), chapterID)
		if err != nil || status.TitleID != titleID || !titleAllowed(r.Context(), u.svc, titleID) {
			http.NotFound(w, r)
			return
		}
		if read {
			_, err = u.svc.MarkChapterRead(r.Context(), chapterID)
			if err == nil {
				u.svc.PushAniListEntry(r.Context(), auth.UserID(r.Context()), titleID)
			}
		} else {
			_, err = u.svc.MarkChapterUnread(r.Context(), chapterID)
			if err == nil {
				u.svc.PushAniListEntryExact(r.Context(), auth.UserID(r.Context()), titleID)
			}
		}
		if err != nil {
			u.fail(w, err)
			return
		}
		u.writeChaptersTable(w, r, titleID)
	}
}

func (u *webUI) chapterRangeRead(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	switch r.FormValue("action") {
	case "read":
		_, err = u.svc.MarkChapterRangeRead(r.Context(), titleID, r.FormValue("from"), r.FormValue("to"))
		if err == nil {
			u.svc.PushAniListEntry(r.Context(), auth.UserID(r.Context()), titleID)
		}
	case "unread":
		_, err = u.svc.MarkChapterRangeUnread(r.Context(), titleID, r.FormValue("from"), r.FormValue("to"))
		if err == nil {
			u.svc.PushAniListEntryExact(r.Context(), auth.UserID(r.Context()), titleID)
		}
	default:
		err = fmt.Errorf("unknown read action")
	}
	if err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, titleID)
}

// chapterRemove deletes one chapter's downloaded file so it shows as missing.
func (u *webUI) chapterRemove(w http.ResponseWriter, r *http.Request) {
	titleID, chapterID, ok := u.chapterOwned(w, r)
	if !ok {
		return
	}
	if err := u.svc.RemoveChapterDownload(r.Context(), chapterID); err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, titleID)
}

// chapterRenameDialog returns the rename form pre-filled with the chapter's title.
func (u *webUI) chapterRenameDialog(w http.ResponseWriter, r *http.Request) {
	_, chapterID, ok := u.chapterOwned(w, r)
	if !ok {
		return
	}
	st, err := u.svc.ChapterReadStatus(r.Context(), chapterID)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "chapterRenameDialog", st)
}

// chapterRename edits one chapter's descriptive title.
func (u *webUI) chapterRename(w http.ResponseWriter, r *http.Request) {
	titleID, chapterID, ok := u.chapterOwned(w, r)
	if !ok {
		return
	}
	if err := u.svc.RenameChapter(r.Context(), chapterID, r.FormValue("title")); err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, titleID)
}

// chapterResetName restores one manually renamed chapter's scraped name.
func (u *webUI) chapterResetName(w http.ResponseWriter, r *http.Request) {
	titleID, chapterID, ok := u.chapterOwned(w, r)
	if !ok {
		return
	}
	if err := u.svc.ResetChapterTitle(r.Context(), chapterID); err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, titleID)
}

// chapterResetNames restores scraped names for the whole title (blank range)
// or an inclusive chapter range.
func (u *webUI) chapterResetNames(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	if _, err := u.svc.ResetChapterTitles(r.Context(), titleID, r.FormValue("from"), r.FormValue("to")); err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, titleID)
}

// chapterDeleteRange deletes downloaded chapters in a whole-number range.
func (u *webUI) chapterDeleteRange(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	if _, err := u.svc.RemoveChapterDownloadsRange(r.Context(), titleID, r.FormValue("from"), r.FormValue("to")); err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, titleID)
}

// chapterOwned resolves and authorizes a {id}/chapters/{chapterID} request.
func (u *webUI) chapterOwned(w http.ResponseWriter, r *http.Request) (titleID, chapterID int64, ok bool) {
	titleID, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return 0, 0, false
	}
	chapterID, err = parseInt64Path(r, "chapterID")
	if err != nil {
		u.fail(w, err)
		return 0, 0, false
	}
	st, err := u.svc.ChapterReadStatus(r.Context(), chapterID)
	if err != nil || st.TitleID != titleID || !titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return 0, 0, false
	}
	return titleID, chapterID, true
}

func (u *webUI) writeChaptersTable(w http.ResponseWriter, r *http.Request, titleID int64) {
	title, err := u.svc.GetTitle(r.Context(), titleID)
	if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		http.NotFound(w, r)
		return
	}
	u.frag(w, "chaptersList", u.buildChaptersTable(r.Context(), title, r.URL.Query()))
}

// jobGroupView labels a global job with its spawned-children counts.
type jobGroupView struct {
	jobs.Job
	Children       int
	ActiveChildren int
}

// jobActionsView pairs a job with whether its cancel action is available.
type jobActionsView struct {
	Job       jobs.Job
	CanCancel bool
}

// chaptersView drives the custom chapters list (reuses tableData paging).
type chaptersView struct {
	tableData
	Chapters []chapterRowView
	Poll     bool // a job for this title is active: keep the list fresh
}

type chapterRowView struct {
	library.ChapterStatus
	Percent   int
	ReadTip   string // e.g. "7/12 pages read"
	Size      string
	When      string
	Tint      string
	CanManage bool // remove-from-disk / rename actions
}

func (u *webUI) buildChaptersTable(ctx context.Context, title library.Title, values url.Values) chaptersView {
	page, key, dir := tableParams(values, chaptersPerPage)
	// The sort dropdown submits one combined "order" value, e.g. "read-desc".
	if o := values.Get("order"); o != "" {
		if k, d, ok := strings.Cut(o, "-"); ok {
			key, dir = k, d
		}
	}
	if key == "" {
		key = "number"
	}
	chs, _ := u.svc.TitleChapters(ctx, title.ID)
	chs = filterChapters(chs, values.Get("q"))
	sortChapters(chs, key, dir)
	rows, total := paginate(chs, page, chaptersPerPage)

	empty := "Link a source to discover chapters."
	if strings.HasPrefix(title.SourceURL, "http") {
		empty = "No chapters yet — refresh to discover them."
	}
	t := chaptersView{tableData: tableData{
		ID: "chapters-table", BaseURL: fmt.Sprintf("/ui/library/%d/chapters", title.ID),
		Page: page, PerPage: chaptersPerPage, Total: total, Sort: key, Dir: dir, Empty: empty,
		Params: chapterTableParams(values),
	}}
	running, _, queued, _, _ := titleActivityFrom(u.jobs(ctx, title.ID), title)
	t.Poll = len(running) > 0 || len(queued) > 0
	canManage := auth.FromContext(ctx).Can(auth.PermLibraryManage)
	for _, c := range rows {
		size, when := "—", ""
		if c.Downloaded {
			size = util.Human(c.Bytes)
			if c.DownloadedAt != nil {
				when = c.DownloadedAt.Local().Format("02 Jan 2006")
			}
		}
		t.Chapters = append(t.Chapters, chapterRowView{
			ChapterStatus: c,
			Percent:       chapterReadPercent(c),
			ReadTip:       chapterReadTip(c),
			Size:          size,
			When:          when,
			Tint:          chapterRowClass(c),
			CanManage:     canManage,
		})
	}
	return t
}

// sourceView builds the candidate-source list for a title, excluding sources
// already linked.
func (u *webUI) sourceView(ctx context.Context, title library.Title, linked map[string]bool, all []sources.Source) matchView {
	if title.CatalogMangaID == nil {
		return titleSourceView(title, false, false, "", nil)
	}
	cid := *title.CatalogMangaID
	active, failed, msg := u.jobStateFor(ctx, jobs.TypeMatchSources, service.JobPayload{CatalogID: cid})
	if active {
		return titleSourceView(title, true, false, "", nil)
	}
	matches, _ := u.svc.ListMatches(ctx, cid)
	// Only offer sources that are currently usable: stored candidates from
	// sources that were disabled or deleted since must not be linkable.
	usable := map[string]bool{}
	for _, src := range all {
		if src.Enabled {
			usable[src.ID] = true
		}
	}
	kept := matches[:0]
	for _, m := range matches {
		if !linked[m.SourceID] && usable[m.SourceID] {
			kept = append(kept, m)
		}
	}
	return titleSourceView(title, false, failed, msg, kept)
}

// linkedSourceIDs is the set of source IDs already linked to a title.
func (u *webUI) linkedSourceIDs(ctx context.Context, titleID int64) map[string]bool {
	links, _ := u.svc.ListTitleSources(ctx, titleID)
	m := make(map[string]bool, len(links))
	for _, l := range links {
		if l.SourceID != "" {
			m[l.SourceID] = true
		}
	}
	return m
}

// linkedSourceViews resolves each linked source's display name and active state.
func (u *webUI) linkedSourceViews(ctx context.Context, title library.Title) []linkedSourceView {
	links, _ := u.svc.ListTitleSources(ctx, title.ID)
	out := make([]linkedSourceView, 0, len(links))
	for _, l := range links {
		name := l.SourceID
		if src, err := u.svc.GetSource(ctx, l.SourceID); err == nil && src.Name != "" {
			name = src.Name
		}
		out = append(out, linkedSourceView{Name: name, SourceID: l.SourceID, URL: l.URL, Active: l.URL == title.SourceURL})
	}
	return out
}

func filterSources(list []sources.Source, linked map[string]bool) []sources.Source {
	out := make([]sources.Source, 0, len(list))
	for _, s := range list {
		if !linked[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

func (u *webUI) sourcesPage(w http.ResponseWriter, r *http.Request) {
	srcs, err := u.svc.ListSources(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	srcs = filterNSFWSources(r.Context(), srcs)
	main, kot := splitKotatsuSources(u.sourceRows(r.Context(), srcs))
	u.page(w, r, "sources", "Sources", sourcesPageView{
		Sources:        main,
		KotatsuSources: kot,
		Scrapers:       registry.Names(),
		KotatsuEnabled: u.svc.KotatsuClient(r.Context()) != nil,
	})
}

type sourcesPageView struct {
	Sources        []sourceRowView
	KotatsuSources []sourceRowView
	Scrapers       []string
	KotatsuEnabled bool
}

type backupsView struct {
	Backups []service.BackupInfo
}

func (u *webUI) backupsPage(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, "backups", "Backups", u.backupsView(r.Context()))
}

func (u *webUI) backupsFrag(w http.ResponseWriter, r *http.Request) {
	u.frag(w, "backupsContent", u.backupsView(r.Context()))
}

func (u *webUI) backupsView(ctx context.Context) backupsView {
	backups, _ := u.svc.ListBackups(ctx)
	return backupsView{Backups: backups}
}

func (u *webUI) backupCreate(w http.ResponseWriter, r *http.Request) {
	if _, err := u.svc.CreateBackup(r.Context()); err != nil {
		u.fail(w, err)
		return
	}
	u.backupsFrag(w, r)
}

func (u *webUI) backupUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		u.fail(w, err)
		return
	}
	file, header, err := r.FormFile("backup")
	if err != nil {
		u.fail(w, err)
		return
	}
	defer file.Close()
	if err := u.svc.UploadBackup(r.Context(), header.Filename, file); err != nil {
		u.fail(w, err)
		return
	}
	u.backupsFrag(w, r)
}

func (u *webUI) backupRestore(w http.ResponseWriter, r *http.Request) {
	if err := u.svc.RestoreBackup(r.Context(), r.PathValue("name")); err != nil {
		u.fail(w, err)
		return
	}
	u.backupsFrag(w, r)
}

func (u *webUI) backupDelete(w http.ResponseWriter, r *http.Request) {
	if err := u.svc.DeleteBackup(r.Context(), r.PathValue("name")); err != nil {
		u.fail(w, err)
		return
	}
	u.backupsFrag(w, r)
}

func (u *webUI) backupDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, err := u.svc.BackupPath(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeFile(w, r, p)
}

func (u *webUI) settingsPage(w http.ResponseWriter, r *http.Request) {
	view := u.settings(r.Context())
	view.AniList = u.svc.AniListConnectionFor(r.Context(), auth.UserID(r.Context()))
	view.RedirectURL = anilistRedirectURL(r)
	view.AppConfigured = u.svc.Setting(r.Context(), service.SettingAniListClientID, "") != ""
	u.page(w, r, "settings", "Settings", view)
}

// --- search & add ---

type searchResultView struct {
	catalog.Manga
	TitleID int64 // set when the manga is already in the library
}

func (v searchResultView) RelationLabel() string {
	labels := map[string]string{
		"SEQUEL": "Sequel", "PREQUEL": "Prequel", "PARENT": "Parent story",
		"SIDE_STORY": "Side story", "SPIN_OFF": "Spin-off", "ALTERNATIVE": "Alternative version",
		"ADAPTATION": "Adaptation", "SOURCE": "Source material", "SUMMARY": "Summary",
		"COMPILATION": "Compilation", "CONTAINS": "Contains", "CHARACTER": "Shared character",
	}
	if label := labels[v.RelationType]; label != "" {
		return label
	}
	return "Direct relation"
}

type searchView struct {
	Controls   searchControls
	TagOptions []catalog.ContentTag
}

type searchControls struct {
	Q           string
	Sort        string // "" relevance, rating, title, year, chapters
	Dir         string
	View        string
	IncludeTags []string
	ExcludeTags []string
}

func searchControlsFrom(values url.Values) searchControls {
	c := searchControls{
		Q:           strings.TrimSpace(values.Get("q")),
		Sort:        values.Get("sort"),
		Dir:         values.Get("dir"),
		View:        values.Get("view"),
		IncludeTags: tagList(values["include_tags"]),
		ExcludeTags: tagList(values["exclude_tags"]),
	}
	switch c.Sort {
	case "rating", "title", "year", "chapters":
	default:
		c.Sort = ""
	}
	if c.Dir != "asc" {
		c.Dir = "desc"
	}
	if c.View != "table" && c.View != "full" {
		c.View = "cards"
	}
	return c
}

// anilistSort maps a sort control to an AniList MediaSort enum.
func anilistSort(key, dir string) string {
	enum, ok := map[string]string{
		"rating":   "SCORE",
		"title":    "TITLE_ROMAJI",
		"year":     "START_DATE",
		"chapters": "CHAPTERS",
	}[key]
	if !ok {
		return ""
	}
	if dir == "desc" {
		return enum + "_DESC"
	}
	return enum
}

// splitByKind partitions values into genres and tags; unknown names are tags.
func splitByKind(names []string, options []catalog.ContentTag) (genres, tags []string) {
	kinds := make(map[string]string, len(options))
	for _, o := range options {
		kinds[strings.ToLower(o.Name)] = o.Kind
	}
	for _, n := range names {
		if kinds[strings.ToLower(strings.TrimSpace(n))] == "genre" {
			genres = append(genres, n)
		} else {
			tags = append(tags, n)
		}
	}
	return genres, tags
}

// filterManga keeps entries carrying every include tag and no exclude tag.
func filterManga(items []catalog.Manga, c searchControls) []catalog.Manga {
	out := make([]catalog.Manga, 0, len(items))
	for _, m := range items {
		tags := mangaContentTags(m)
		keep := !library.HasAnyTag(tags, c.ExcludeTags)
		for _, inc := range c.IncludeTags {
			if !keep {
				break
			}
			keep = library.HasAnyTag(tags, []string{inc})
		}
		if keep {
			out = append(out, m)
		}
	}
	return out
}

// filterSortManga additionally sorts; only for unsorted recommendation pools.
func filterSortManga(items []catalog.Manga, c searchControls) []catalog.Manga {
	out := filterManga(items, c)
	chapters := func(m catalog.Manga) int {
		if m.Chapters == nil {
			return 0
		}
		return *m.Chapters
	}
	var less func(a, b catalog.Manga) bool
	switch c.Sort {
	case "rating":
		less = func(a, b catalog.Manga) bool { return a.AverageScore < b.AverageScore }
	case "title":
		less = func(a, b catalog.Manga) bool { return strings.ToLower(mangaTitle(a)) < strings.ToLower(mangaTitle(b)) }
	case "year":
		less = func(a, b catalog.Manga) bool { return a.Year < b.Year }
	case "chapters":
		less = func(a, b catalog.Manga) bool { return chapters(a) < chapters(b) }
	default:
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c.Dir == "desc" {
			return less(out[j], out[i])
		}
		return less(out[i], out[j])
	})
	return out
}

func (u *webUI) searchPage(w http.ResponseWriter, r *http.Request) {
	view := searchView{Controls: searchControlsFrom(r.URL.Query())}
	options, _ := u.svc.StoredContentTags(r.Context())
	view.TagOptions = visibleTagOptions(r.Context(), options)
	u.page(w, r, "search", "Search", view)
}

// searchResultsView is one result page plus the "Show more" continuation.
type searchResultsView struct {
	Results   mangaResults
	Controls  searchControls
	View      string
	NextPage  int  // 0 = no further pages
	RetryPage int  // set with Error: the page to retry
	Append    bool // page 2+: suppress the empty-state text
	Error     bool
}

const searchPerPage = 18

func (u *webUI) search(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c := searchControlsFrom(r.Form)
	filtered := len(c.IncludeTags) > 0 || len(c.ExcludeTags) > 0
	if c.Q == "" && !filtered {
		u.trendingManga(w, r)
		return
	}
	page, _ := strconv.Atoi(r.FormValue("page"))
	if page < 1 {
		page = 1
	}
	filter := catalog.SearchFilter{Sort: anilistSort(c.Sort, c.Dir)}
	if c.Q == "" && filter.Sort == "" {
		filter.Sort = "POPULARITY_DESC"
	}
	if filtered {
		if options, err := u.svc.ContentTagOptions(r.Context()); err == nil && len(options) > 0 {
			filter.GenreIn, filter.TagIn = splitByKind(c.IncludeTags, options)
			filter.GenreNotIn, filter.TagNotIn = splitByKind(c.ExcludeTags, options)
		}
	}
	view := searchResultsView{Controls: c, View: c.View, Append: page > 1}
	var items []catalog.Manga
	more, last := false, page
	for scan := 0; scan < 3; scan++ {
		last = page + scan
		filter.Page = last
		fetched, m, err := u.svc.SearchAniList(r.Context(), c.Q, searchPerPage, filter)
		if err != nil {
			log.Printf("anilist search failed: %v", err)
			view.Error, view.RetryPage = true, page
			u.frag(w, "searchResults", view)
			return
		}
		items, more = filterManga(fetched, c), m
		if len(items) > 0 || !more {
			break
		}
	}
	view.Results = u.mangaResultsView(r.Context(), "", view.View, items)
	if more {
		view.NextPage = last + 1
	}
	u.frag(w, "searchResults", view)
}

func (u *webUI) addToLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.FormValue("provider_id"))
	if err != nil {
		u.fail(w, fmt.Errorf("invalid id"))
		return
	}
	title, err := u.svc.AddCatalogTitle(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		u.fail(w, fmt.Errorf("title not found"))
		return
	}
	u.kick()
	if r.FormValue("redirect") != "" {
		w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", title.ID))
		return
	}
	if r.FormValue("card") != "" {
		u.frag(w, "addedButtonCard", title)
		return
	}
	u.frag(w, "addedButton", title)
}

// --- library ---

func (u *webUI) libAction(typ, label string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			u.fail(w, err)
			return
		}
		title, err := u.svc.GetTitle(r.Context(), id)
		if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
			u.fail(w, fmt.Errorf("title not found"))
			return
		}
		if _, err := u.svc.Enqueue(r.Context(), typ, id, time.Now()); err != nil {
			u.fail(w, err)
			return
		}
		u.kick()
		w.Header().Set("HX-Trigger", "title-job-started") // wakes the title page's idle pollers
		view := u.titleActivity(r.Context(), id)
		view.Title = title
		if view.Running == nil {
			view.Running = map[string]bool{typ: true}
			view.ActiveLabel = label
		}
		u.frag(w, "titleActivity", view)
	}
}

func (u *webUI) libraryBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	ids, err := selectedTitleIDs(r.Form["title_id"])
	if err != nil {
		u.fail(w, err)
		return
	}
	action := r.FormValue("action")
	switch action {
	case "refresh", "download", "scan", "monitor-on", "monitor-off", "remove":
	default:
		u.fail(w, fmt.Errorf("unknown bulk action"))
		return
	}
	var errs []error
	for _, id := range ids {
		title, err := u.svc.GetTitle(r.Context(), id)
		if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
			errs = append(errs, fmt.Errorf("title %d not found", id))
			continue
		}
		switch action {
		case "refresh":
			_, err = u.svc.Enqueue(r.Context(), jobs.TypeRefreshTitle, id, time.Now())
		case "download":
			_, err = u.svc.Enqueue(r.Context(), jobs.TypeDownloadMissing, id, time.Now())
		case "scan":
			_, err = u.svc.Enqueue(r.Context(), jobs.TypeScanDownloads, id, time.Now())
		case "monitor-on":
			err = u.svc.SetMonitored(r.Context(), id, true)
		case "monitor-off":
			err = u.svc.SetMonitored(r.Context(), id, false)
		case "remove":
			_, err = u.svc.RemoveTitleFiles(r.Context(), id, false, false)
		}
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	u.frag(w, "libraryResults", u.buildLibraryTable(r.Context(), r.Form))
}

func selectedTitleIDs(values []string) ([]int64, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("select at least one title")
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid title id %q", value)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (u *webUI) libActivity(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	view := u.titleActivity(r.Context(), id)
	if len(view.Running) == 0 {
		// Job finished: reload so counts, progress, and the chapter list refresh.
		w.Header().Set("HX-Refresh", "true")
		return
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	view.Title = title
	u.frag(w, "titleActivity", view)
}

func (u *webUI) libRefreshInterval(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	if err := u.svc.SetRefreshInterval(r.Context(), id, r.FormValue("interval")); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", id))
}

func (u *webUI) libMonitored(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	on := r.FormValue("on") == "true"
	if err := u.svc.SetMonitored(r.Context(), id, on); err != nil {
		u.fail(w, err)
		return
	}
	title, _ := u.svc.GetTitle(r.Context(), id)
	u.frag(w, "monitorToggle", title)
}

func (u *webUI) libRemove(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	if _, err := u.svc.RemoveTitleFiles(r.Context(), id, r.FormValue("delete_files") == "on", r.FormValue("delete_anilist") == "on"); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/library")
	w.WriteHeader(http.StatusOK)
}

// titleDialogView backs the shared remove/settings modals used from list views,
// where per-card modals would collide on id.
type titleDialogView struct {
	Title            library.Title
	AniListConnected bool
	RefreshEvery     string
}

func (u *webUI) titleDialog(r *http.Request) (titleDialogView, error) {
	id, err := pathID(r)
	if err != nil {
		return titleDialogView{}, err
	}
	t, err := u.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), t.IsAdult, t.ContentTags) {
		return titleDialogView{}, fmt.Errorf("title not found")
	}
	return titleDialogView{
		Title:            t,
		AniListConnected: u.svc.AniListConnectionFor(r.Context(), auth.UserID(r.Context())).Connected,
		RefreshEvery:     u.svc.Setting(r.Context(), service.SettingServeRefreshEvery, service.SettingDefault(service.SettingServeRefreshEvery)),
	}, nil
}

func (u *webUI) titleRemoveDialog(w http.ResponseWriter, r *http.Request) {
	v, err := u.titleDialog(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "removeTitleDialog", v)
}

func (u *webUI) titleSettingsDialog(w http.ResponseWriter, r *http.Request) {
	v, err := u.titleDialog(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "titleSettingsDialog", v)
}

// --- import & source linking ---

type importPickerView struct {
	Folder  string
	Query   string
	Results []catalog.Manga
}

func (u *webUI) importPage(w http.ResponseWriter, r *http.Request) {
	u.page(w, r, "import", "Import", nil) // candidates post-load: the dir scan can be slow
}

type importCandidatesView struct {
	Candidates []service.ImportCandidate
	HasTitles  bool
}

func (u *webUI) importCandidates(w http.ResponseWriter, r *http.Request) {
	cands, err := u.svc.ExploreDownloads(r.Context())
	if err != nil {
		u.fail(w, err)
		return
	}
	view := importCandidatesView{Candidates: cands}
	if q, ok := visibleTitles(r.Context()); ok {
		n, _ := u.svc.CountTitles(r.Context(), q)
		view.HasTitles = n > 0
	}
	u.frag(w, "importCandidates", view)
}

func (u *webUI) importTitleOptions(w http.ResponseWriter, r *http.Request) {
	var titles []library.Title
	if q, ok := visibleTitles(r.Context()); ok {
		titles, _ = u.svc.TitleBasics(r.Context(), q)
	}
	u.frag(w, "importTitleOptions", titles)
}

func (u *webUI) importSearch(w http.ResponseWriter, r *http.Request) {
	folder := r.PathValue("folder")
	query := strings.TrimSpace(r.FormValue("q"))
	if query == "" {
		query = importQuery(folder)
	}
	items, _, err := u.svc.SearchAniList(r.Context(), query, 10, catalog.SearchFilter{})
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "importPicker", importPickerView{Folder: folder, Query: query, Results: allowedManga(r.Context(), items)})
}

func (u *webUI) importDo(w http.ResponseWriter, r *http.Request) {
	anilistID, err := strconv.Atoi(r.FormValue("anilist_id"))
	if err != nil {
		u.fail(w, fmt.Errorf("select an AniList match first"))
		return
	}
	var title library.Title
	if r.FormValue("kind") == "volumes" {
		title, err = u.svc.ImportVolumesFolder(r.Context(), r.FormValue("folder"), anilistID)
	} else {
		title, err = u.svc.ImportFolder(r.Context(), r.FormValue("folder"), anilistID)
	}
	if err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", title.ID))
}

func (u *webUI) findSources(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		http.NotFound(w, r)
		return
	}
	if title.CatalogMangaID == nil {
		u.fail(w, fmt.Errorf("no catalog metadata to search from"))
		return
	}
	// An explicit re-search must not echo cached (possibly stale) decisions.
	if err := u.svc.ClearMatches(r.Context(), *title.CatalogMangaID); err != nil {
		u.fail(w, err)
		return
	}
	if _, err := u.svc.EnqueueCatalog(r.Context(), jobs.TypeMatchSources, *title.CatalogMangaID, time.Now()); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	u.frag(w, "matches", titleSourceView(title, true, false, "", nil))
}

func (u *webUI) titleSources(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	title, err := u.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), title.IsAdult, title.ContentTags) {
		u.frag(w, "matches", matchView{DomID: fmt.Sprintf("sources-%d", id), PollURL: fmt.Sprintf("/ui/library/%d/sources", id)})
		return
	}
	allSources, _ := u.svc.ListSources(r.Context())
	u.frag(w, "matches", u.sourceView(r.Context(), title, u.linkedSourceIDs(r.Context(), id), allSources))
}

func (u *webUI) unlinkSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	if err := u.svc.UnlinkTitleSource(r.Context(), id, r.FormValue("url")); err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", id))
}

func (u *webUI) linkSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	matchID, err := strconv.ParseInt(r.FormValue("match_id"), 10, 64)
	if err != nil {
		u.fail(w, fmt.Errorf("invalid match id"))
		return
	}
	if _, err := u.svc.LinkTitleSource(r.Context(), id, matchID); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", id))
}

func (u *webUI) srcVerifyURL(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	sourceID := strings.TrimSpace(r.FormValue("source_id"))
	rawURL := strings.TrimSpace(r.FormValue("url"))
	res, err := u.svc.VerifySourceURL(r.Context(), sourceID, rawURL)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "sourceProbe", sourceProbeView{TitleID: id, SourceID: sourceID, URL: rawURL, Result: res})
}

func (u *webUI) linkSourceURL(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	if _, err := u.svc.LinkTitleSourceURL(r.Context(), id, r.FormValue("source_id"), r.FormValue("url")); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", id))
}

func (u *webUI) linkSourceByID(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if !titleAllowed(r.Context(), u.svc, id) {
		http.NotFound(w, r)
		return
	}
	sourceID := strings.TrimSpace(r.FormValue("source_id"))
	if sourceID == "" {
		u.fail(w, fmt.Errorf("pick a source"))
		return
	}
	if _, err := u.svc.LinkTitleToSource(r.Context(), id, sourceID); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", id))
}

func importQuery(folder string) string {
	return strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(folder))
}

// --- sources ---

func (u *webUI) srcVerify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := u.svc.EnqueueSource(r.Context(), id, time.Now()); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	src, err := u.svc.GetSource(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "sourceRow", sourceRowView{Source: src, Active: true, Poll: true, Titles: u.sourceTitleCount(r.Context(), id), FetchFails: u.sourceFetchFailCount(r.Context(), id)})
}

func (u *webUI) srcEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := u.svc.SetSourceEnabled(r.Context(), id, r.FormValue("on") == "true"); err != nil {
		u.fail(w, err)
		return
	}
	u.srcRow(w, r)
}

func (u *webUI) srcDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := u.svc.RemoveLocalSource(r.Context(), id); err != nil {
		u.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// sourceRows marks rows with a queued/running verify job so they render the
// verifying spinner.
func (u *webUI) sourceRows(ctx context.Context, srcs []sources.Source) []sourceRowView {
	verifying := map[string]bool{}
	active, _ := u.svc.FindJobs(ctx, jobs.Filter{Type: jobs.TypeVerifySource, Statuses: []string{"queued", "running"}})
	for _, j := range active {
		var p service.JobPayload
		if json.Unmarshal([]byte(j.Payload), &p) == nil {
			verifying[p.SourceID] = true
		}
	}
	counts, _ := u.svc.SourceTitleCounts(ctx)
	fails, _ := u.svc.SourceFetchFailureCounts(ctx)
	rows := make([]sourceRowView, len(srcs))
	for i, s := range srcs {
		rows[i] = sourceRowView{Source: s, Active: verifying[s.ID], Titles: counts[s.ID], FetchFails: fails[s.ID]}
	}
	return rows
}

func splitKotatsuSources(rows []sourceRowView) (main, kot []sourceRowView) {
	for _, row := range rows {
		if strings.HasPrefix(row.Source.Scraper, kotatsu.ScraperPrefix) {
			kot = append(kot, row)
		} else {
			main = append(main, row)
		}
	}
	return main, kot
}

func (u *webUI) srcRow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, err := u.svc.GetSource(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	active, _, _ := u.jobStateFor(r.Context(), jobs.TypeVerifySource, service.JobPayload{SourceID: id})
	u.frag(w, "sourceRow", sourceRowView{Source: src, Active: active, Poll: active, Titles: u.sourceTitleCount(r.Context(), id), FetchFails: u.sourceFetchFailCount(r.Context(), id)})
}

func (u *webUI) sourceTitleCount(ctx context.Context, id string) int {
	counts, _ := u.svc.SourceTitleCounts(ctx)
	return counts[id]
}

func (u *webUI) sourceFetchFailCount(ctx context.Context, id string) int {
	fails, _ := u.svc.SourceFetchFailureCounts(ctx)
	return fails[id]
}

// srcTitleErrors lists this source's titles whose last refresh failed.
func (u *webUI) srcTitleErrors(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	failing, err := u.svc.FailingTitles(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "sourceTitleErrors", failing)
}

// sourceEditView carries the scraper choices next to the profile fields.
type sourceEditView struct {
	sources.Source
	Scrapers []string
	Kotatsu  bool
}

func (u *webUI) srcEdit(w http.ResponseWriter, r *http.Request) {
	src, err := u.svc.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		u.fail(w, err)
		return
	}
	names := registry.Names()

	if !slices.Contains(names, src.Scraper) && src.Scraper != "" {
		names = append([]string{src.Scraper}, names...)
	}
	u.frag(w, "sourceEdit", sourceEditView{
		Source: src, Scrapers: names,
		Kotatsu: strings.HasPrefix(src.Scraper, kotatsu.ScraperPrefix),
	})
}

func (u *webUI) srcEditSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, err := u.svc.GetSource(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	_ = r.ParseForm()
	p := src.Profile
	p.Name = strings.TrimSpace(r.FormValue("name"))
	p.BaseURL = strings.TrimSpace(r.FormValue("base_url"))
	p.SampleMangaURL = strings.TrimSpace(r.FormValue("manga_url"))
	p.Domains = splitList(r.FormValue("domains"))
	p.Scraper = strings.TrimSpace(r.FormValue("scraper"))
	if r.Form.Has("search_url") {
		p.SearchURL = strings.TrimSpace(r.FormValue("search_url"))
	}
	if r.Form.Has("extensions") {
		p.AllowedExtensions = splitList(r.FormValue("extensions"))
	}
	if r.Form.Has("languages") {
		p.Languages = splitList(r.FormValue("languages"))
	}
	p.SingleManga = formChecked(r, "single_manga")
	p.Chapterless = formChecked(r, "chapterless")
	p.NSFW = formChecked(r, "nsfw")
	p.Enabled = formChecked(r, "enabled")
	// Saving stores a local override; built-in sync no longer clobbers it.
	if err := u.svc.ImportLocalSource(r.Context(), p); err != nil {
		u.fail(w, err)
		return
	}
	// The sync upsert deliberately preserves the stored enabled flag, so the
	// checkbox needs its own write.
	if err := u.svc.SetSourceEnabled(r.Context(), id, p.Enabled); err != nil {
		u.fail(w, err)
		return
	}
	if parserID, ok := strings.CutPrefix(p.Scraper, kotatsu.ScraperPrefix); ok && len(p.Domains) > 0 {
		if kc := u.svc.KotatsuClient(r.Context()); kc != nil {
			if err := kc.SetConfig(r.Context(), parserID, map[string]string{"domain": p.Domains[0]}); err != nil {
				u.fail(w, fmt.Errorf("saved, but the worker rejected the domain: %w", err))
				return
			}
		}
	}
	if r.Form.Has("chapter_fetch") || r.Form.Has("image_fetch") {
		if err := u.svc.SetSourceMethods(r.Context(), id, r.FormValue("chapter_fetch"), r.FormValue("image_fetch")); err != nil {
			u.fail(w, err)
			return
		}
	}
	u.kick()
	w.Header().Set("HX-Refresh", "true")
}

// splitCommaList splits on commas only; items like tags may contain spaces.
func splitCommaList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (u *webUI) srcSync(w http.ResponseWriter, r *http.Request) {
	registry := u.svc.Setting(r.Context(), service.SettingSourceRegistryURL, "")
	if err := u.svc.SyncSources(r.Context(), registry); err != nil {
		u.fail(w, err)
		return
	}
	u.sourcesTable(w, r)
}

func (u *webUI) sourcesTable(w http.ResponseWriter, r *http.Request) {
	srcs, _ := u.svc.ListSources(r.Context())
	main, _ := splitKotatsuSources(u.sourceRows(r.Context(), filterNSFWSources(r.Context(), srcs)))
	u.frag(w, "sourcesTable", main)
}

func (u *webUI) srcTest(w http.ResponseWriter, r *http.Request) {
	profile, solver, browser, err := customProfileFromForm(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	res, err := u.svc.TestSource(r.Context(), profile, solver, browser)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.frag(w, "sourceTest", res)
}

func (u *webUI) srcAddCustom(w http.ResponseWriter, r *http.Request) {
	profile, solver, browser, err := customProfileFromForm(r)
	if err != nil {
		u.fail(w, err)
		return
	}
	if err := u.svc.ImportLocalSource(r.Context(), profile); err != nil {
		u.fail(w, err)
		return
	}
	// Pin the fetch methods the user chose so verify/downloads honor them.
	chapterFetch, imageFetch := "", ""
	if solver {
		chapterFetch = sources.FetchSolver
	}
	if browser {
		imageFetch = sources.FetchBrowser
	}
	if err := u.svc.SetSourceMethods(r.Context(), profile.ID, chapterFetch, imageFetch); err != nil {
		u.fail(w, err)
		return
	}
	u.kick()
	w.Header().Set("HX-Refresh", "true")
}

// customProfileFromForm builds a local source profile from the add-source form.
func customProfileFromForm(r *http.Request) (sources.Profile, bool, bool, error) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	base := strings.TrimSpace(r.FormValue("base_url"))
	manga := strings.TrimSpace(r.FormValue("manga_url"))
	if base == "" || manga == "" {
		return sources.Profile{}, false, false, fmt.Errorf("site base URL and manga page URL are required")
	}
	var exts []string
	for _, e := range r.Form["ext"] {
		if e = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e)), "."); e != "" {
			exts = append(exts, e)
		}
	}
	host := ""
	if u, err := url.Parse(base); err == nil {
		host = u.Host
	}
	id := slugify(name)
	if id == "" {
		id = slugify(host)
	}
	if id == "" {
		return sources.Profile{}, false, false, fmt.Errorf("a name is required")
	}
	name = orDefault(name, host)
	var domains []string
	if host != "" {
		domains = []string{host}
	}
	solver := formChecked(r, "solver")
	browser := formChecked(r, "browser")
	return sources.Profile{
		ID: id, Name: name, Domains: domains,
		BaseURL: base, SampleMangaURL: manga, Scraper: strings.TrimSpace(r.FormValue("scraper")),
		AllowedExtensions: exts, MinChapters: 1,
		RequiresBrowserSolver: solver, RequiresBrowserDownload: browser,
		SingleManga: formChecked(r, "single_manga"), Chapterless: formChecked(r, "chapterless"), NSFW: formChecked(r, "nsfw"), Enabled: true,
	}, solver, browser, nil
}

func formChecked(r *http.Request, name string) bool {
	switch r.FormValue(name) {
	case "on", "true", "1":
		return true
	}
	return false
}

var reNonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(reNonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// --- settings ---

func (u *webUI) settingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	themeBefore, _ := u.theme(r.Context())
	user := userFrom(r.Context())
	for _, key := range service.SettingKeys() {
		if !r.Form.Has(key) {
			continue // section not shown to this user; leave untouched
		}
		appearance := strings.HasPrefix(key, "ui.")
		if appearance && !user.Can(auth.PermSettingsAppearance) {
			continue
		}
		if !appearance && !user.Can(auth.PermSettingsManage) {
			continue
		}
		value := strings.TrimSpace(r.FormValue(key))
		if key == service.SettingAniListClientSecret && value == redactedSecret {
			continue
		}
		// Personal appearance values are always stored explicitly: "equals the
		// built-in default" must not clear them, or a differing pre-multi-user
		// global value (e.g. an old ui.theme) silently takes over again.
		if appearance {
			if value != "" {
				if err := service.ValidateSetting(key, value); err != nil {
					u.fail(w, err)
					return
				}
			}
			if err := u.svc.SetUserSetting(r.Context(), user.ID, key, value); err != nil {
				u.fail(w, err)
				return
			}
			continue
		}
		// Leaving a global field at its default clears any override so
		// env/config wins (avoids the pre-filled default clobbering e.g. a
		// solver endpoint set via environment).
		if value == "" || value == service.SettingDefault(key) {
			if err := u.svc.ClearSetting(r.Context(), key); err != nil {
				u.fail(w, err)
				return
			}
			continue
		}
		if err := service.ValidateSetting(key, value); err != nil {
			u.fail(w, err)
			return
		}
		if err := u.svc.SetSetting(r.Context(), key, value); err != nil {
			u.fail(w, err)
			return
		}
	}
	// Appearance changes need a reload to re-render with the new theme.
	if themeAfter, _ := u.theme(r.Context()); themeAfter != themeBefore || r.FormValue(service.SettingUITheme) == "custom" {
		w.Header().Set("HX-Refresh", "true")
		return
	}
	u.frag(w, "toast", toastView{OK: true, Msg: "Saved ✓"})
}

// --- helpers ---

func (u *webUI) settings(ctx context.Context) settingsView {
	cfg, _, _ := u.svc.RuntimeConfig(ctx) // effective config (env + settings merged)
	stored := u.svc.AllSettings(ctx)
	for k, v := range u.svc.UserSettings(ctx, auth.UserID(ctx)) {
		stored[k] = v // appearance keys are personal
	}
	field := func(key string) settingField {
		label, desc := settingMeta(key)
		value, ok := stored[key]
		if !ok {
			value = service.SettingDefault(key)
		}
		if cfg != nil {
			if eff, ok := effectiveSetting(cfg, key); ok {
				value = eff // show what's actually in effect (e.g. env-set endpoint)
			}
		}
		f := settingField{Key: key, Label: label, Desc: desc, Value: value}
		switch {
		case key == service.SettingUITheme:
			f.Kind, f.Options = "select", service.UIThemes()
		case strings.HasPrefix(key, "ui.custom."):
			f.Kind = "color"
		case key == service.SettingAniListClientID, key == service.SettingAniListClientSecret:
			f.Kind = "secret"
			if key == service.SettingAniListClientSecret && f.Value != "" {
				f.Value = redactedSecret
			}
		}
		return f
	}
	fields := func(keys ...string) []settingField {
		out := make([]settingField, 0, len(keys))
		for _, k := range keys {
			out = append(out, field(k))
		}
		return out
	}
	colorKeys := make([]string, 0, len(service.CustomColorTokens()))
	for _, t := range service.CustomColorTokens() {
		colorKeys = append(colorKeys, service.CustomColorKey(t))
	}
	user := userFrom(ctx)
	var groups []settingGroup
	if user.Can(auth.PermSettingsAppearance) {
		groups = append(groups, settingGroup{Title: "Appearance", Fields: append(fields(service.SettingUITheme), fields(colorKeys...)...)})
	}
	if user.Can(auth.PermSettingsManage) {
		groups = append(groups,
			settingGroup{Title: "Scheduling", Fields: fields(
				service.SettingServeRefreshEvery, service.SettingServeScanEvery,
				service.SettingServeDownloadEvery, service.SettingServeSourceVerifyEvery,
				service.SettingServeBackupEvery, service.SettingServeRunEvery,
				service.SettingServeAniListSyncEvery,
				service.SettingServeCatalogEvery)},
			settingGroup{Title: "Jobs & downloads", Fields: fields(
				service.SettingJobsMaxAttempts, service.SettingJobsTimeout,
				service.SettingJobsWorkers, service.SettingJobsKeepHistory,
				service.SettingDownloadsMaxAttempts,
				service.SettingRateLimitIntervalMS, service.SettingRateLimitBurst,
				service.SettingRateLimitDisabled)},
			settingGroup{Title: "Services", Fields: fields(
				service.SettingBrowserSolverEnabled, service.SettingBrowserSolverProvider,
				service.SettingBrowserSolverEndpoint, service.SettingBrowserSolverTimeoutSeconds,
				service.SettingBrowserDownloaderEnabled, service.SettingBrowserDownloaderEndpoint,
				service.SettingBrowserDownloaderTimeoutSeconds, service.SettingServicesHealthInterval)},
			settingGroup{Title: "Sources", Fields: fields(service.SettingSourceRegistryURL)},
			settingGroup{Title: "AniList application", Fields: fields(
				service.SettingAniListClientID, service.SettingAniListClientSecret)},
		)
	}
	return settingsView{Groups: groups}
}

// effectiveSetting returns the in-effect value for env-backed settings so the
// form reflects reality rather than the hard-coded default.
func effectiveSetting(cfg *config.Config, key string) (string, bool) {
	switch key {
	case service.SettingBrowserSolverEnabled:
		return strconv.FormatBool(cfg.BrowserSolver.Enabled), true
	case service.SettingBrowserSolverProvider:
		return cfg.BrowserSolver.Provider, true
	case service.SettingBrowserSolverEndpoint:
		return cfg.BrowserSolver.Endpoint, true
	case service.SettingBrowserSolverTimeoutSeconds:
		return strconv.Itoa(cfg.BrowserSolver.TimeoutSeconds), true
	case service.SettingBrowserDownloaderEnabled:
		return strconv.FormatBool(cfg.BrowserDownload.Enabled), true
	case service.SettingBrowserDownloaderEndpoint:
		return cfg.BrowserDownload.Endpoint, true
	case service.SettingBrowserDownloaderTimeoutSeconds:
		return strconv.Itoa(cfg.BrowserDownload.TimeoutSeconds), true
	}
	return "", false
}

// jobState classifies a job status for the UI: active covers pending retries
// (a "failed" job is re-claimed on backoff); dead is the terminal failure.
func jobState(status string) (active, failed bool) {
	switch status {
	case "queued", "running", "failed":
		return true, false
	case "dead":
		return false, true
	}
	return false, false
}

// titleActivity reports which of a title's job types are running (for button
// locking), a verb for the spinner, and the most recent terminal failure.
func (u *webUI) titleActivity(ctx context.Context, id int64) activityView {
	title, err := u.svc.GetTitle(ctx, id)
	if err != nil {
		title = library.Title{ID: id, Monitored: true}
	}
	running, label, queued, failed, msg := titleActivityFrom(u.jobs(ctx, title.ID), title)
	return activityView{Title: title, Running: running, ActiveLabel: label, Queued: queued, Failed: failed, Error: msg}
}

func titleActivityFrom(js []jobs.Job, title library.Title) (running map[string]bool, label string, queued []string, failed bool, msg string) {
	running = map[string]bool{}
	queuedSeen := map[string]bool{}
	for _, j := range js { // List is newest-first
		var p service.JobPayload
		if json.Unmarshal([]byte(j.Payload), &p) != nil {
			continue
		}
		if p.TitleID != title.ID {
			continue
		}
		verb := titleVerb(j.Type)
		if verb == "" {
			continue
		}
		active, isFailed := jobState(j.Status)
		switch {
		case active:
			running[j.Type] = true
			// A running job shows the live spinner; merely waiting jobs are
			// listed as queued.
			if label == "" && j.Status == "running" {
				label = verb
			}
			if j.Status == "queued" && !queuedSeen[j.Type] {
				queuedSeen[j.Type] = true
				queued = append(queued, verb)
			}
		case isFailed && !failed:
			failed, msg = true, j.LastError
		}
	}
	if len(running) > 0 {
		failed = false // something is running; don't also show the last failure
	}
	return running, label, queued, failed, msg
}

func titleVerb(typ string) string {
	switch typ {
	case jobs.TypeRefreshTitle:
		return "refreshing"
	case jobs.TypeDownloadMissing:
		return "downloading"
	case jobs.TypeBackupUserData:
		return "backing up"
	case jobs.TypeScanDownloads:
		return "scanning"
	case jobs.TypeAttachVolumes:
		return "attaching volumes"
	case jobs.TypeDetectCrops:
		return "detecting crops"
	}
	return ""
}

// jobStateFor reports the newest job of one type matching payload.
func (u *webUI) jobStateFor(ctx context.Context, typ string, payload service.JobPayload) (active, failed bool, msg string) {
	return jobStateForSvc(ctx, u.svc, typ, payload)
}

func jobStateForSvc(ctx context.Context, svc *service.JobService, typ string, payload service.JobPayload) (active, failed bool, msg string) {
	want, _ := json.Marshal(payload)
	list, _ := svc.FindJobs(ctx, jobs.Filter{Type: typ, Payload: string(want), Limit: 1})
	if len(list) == 0 {
		return false, false, ""
	}
	active, failed = jobState(list[0].Status)
	return active, failed, list[0].LastError
}

func (u *webUI) jobs(ctx context.Context, titleIDs ...int64) []jobs.Job {
	list, _ := u.svc.FindJobs(ctx, jobs.Filter{TitleIDs: titleIDs, Newest: true})
	return list
}

func navFor(path string) string {
	switch {
	case path == "/":
		return "home"
	case strings.HasPrefix(path, "/management"):
		return "management"
	case strings.HasPrefix(path, "/metrics"):
		return "metrics"
	case strings.HasPrefix(path, "/search"):
		return "search"
	case strings.HasPrefix(path, "/library"):
		return "library"
	case strings.HasPrefix(path, "/collections"):
		return "collections"
	case strings.HasPrefix(path, "/import"):
		return "import"
	case strings.HasPrefix(path, "/sources"):
		return "sources"
	case strings.HasPrefix(path, "/backups"):
		return "backups"
	case strings.HasPrefix(path, "/users"):
		return "users"
	case strings.HasPrefix(path, "/settings"):
		return "settings"
	}
	return ""
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (u *webUI) funcs() template.FuncMap {
	return template.FuncMap{
		"assetVer": func() string { return u.assetVer },
		"chname":   chapters.DisplayName,
		"desc":     util.PlainDescription,
		"jobLabel": jobLabel,
		"tokenExpired": func(expiresAt string) bool {
			t, err := database.ParseTime(expiresAt)
			return err == nil && !t.IsZero() && t.Before(time.Now())
		},
		"lineChart":  lineChart,
		"heatmap":    heatmap,
		"donut":      donut,
		"donutColor": donutColor,
		"f1":         func(x float64) string { return fmt.Sprintf("%.1f", x) },
		"mul60":      func(m int64) int64 { return m * 60 },
		"hdur": func(sec int64) string {
			switch {
			case sec >= 86400:
				return fmt.Sprintf("%dd %dh", sec/86400, (sec%86400)/3600)
			case sec >= 3600:
				return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
			default:
				return fmt.Sprintf("%dm", sec/60)
			}
		},
		"maxCount": func(list []service.NamedCount) int64 {
			var m int64 = 1
			for _, c := range list {
				if c.Count > m {
					m = c.Count
				}
			}
			return m
		},
		"peakDay": func(list []service.DayCount) int64 {
			var m int64
			for _, c := range list {
				if c.Count > m {
					m = c.Count
				}
			}
			return m
		},
		"permLabel": func(p string) string { l, _ := permMeta(p); return l },
		"cardView": func(t library.Title, canManage bool, screen int64) map[string]any {
			return map[string]any{"Title": t, "CanManage": canManage, "Screen": screen}
		},
		"permDesc": func(p string) string { _, d := permMeta(p); return d },
		"has": func(list []string, v string) bool {
			for _, s := range list {
				if s == v {
					return true
				}
			}
			return false
		},
		"mangaTitle": mangaTitle,
		"sub": func(a, b int64) int64 {
			if a < b {
				return 0
			}
			return a - b
		},
		"bar": func(readPct, fullPct int64) map[string]int64 {
			return map[string]int64{"Read": readPct, "Full": fullPct}
		},
		"tagPicker": func(options []catalog.ContentTag, values []string, name, label, hint string) map[string]any {
			// Keys canonicalize to option names; unmatched values become Extras.
			selected := make(map[string]bool, len(values))
			var extras []string
			for _, v := range values {
				key := strings.TrimSpace(v)
				if key == "" {
					continue
				}
				matched := false
				for _, o := range options {
					if strings.EqualFold(o.Name, key) {
						key = o.Name
						matched = true
						break
					}
				}
				if !matched && len(options) > 0 && !selected[key] {
					extras = append(extras, key)
				}
				selected[key] = true
			}
			var chosen, rest []catalog.ContentTag
			for _, o := range options {
				if selected[o.Name] {
					chosen = append(chosen, o)
				} else {
					rest = append(rest, o)
				}
			}
			return map[string]any{"HasOptions": len(options) > 0, "SelectedOptions": chosen, "Options": rest, "Selected": selected, "Extras": extras, "Values": values, "Name": name, "Label": label, "Hint": hint}
		},
		"tagFilter": tagFilterView,
		"globalJobs": func() []map[string]string {
			out := make([]map[string]string, 0, len(globalJobTypes))
			for _, typ := range globalJobTypes {
				out = append(out, map[string]string{"Type": typ, "Label": jobLabel(typ)})
			}
			return out
		},
		"since":      since,
		"confidence": func(c float64) string { return fmt.Sprintf("%.0f%%", c*100) },
		"orUnknown":  func(s string) string { return orDefault(s, "unknown") },
		"orDash":     func(s string) string { return orDefault(s, "—") },
		"linked":     func(s string) bool { return strings.HasPrefix(s, "http") },
		"imported":   func(s string) bool { return strings.HasPrefix(s, "local:") },
		"pathEscape": url.PathEscape,
		"humanBytes": util.Human,
		"releaseStatus": func(s string) string {
			return strings.ToLower(strings.ReplaceAll(s, "_", " "))
		},
		"pct": func(done, total int64) int64 { return percent(done, total) },
		"missingTotal": func(ts []library.Title) int64 {
			var n int64
			for _, t := range ts {
				n += t.MissingCount
			}
			return n
		},
		"healthyCount": func(ss []sources.Source) int {
			n := 0
			for _, s := range ss {
				if s.Status == sources.StatusHealthy {
					n++
				}
			}
			return n
		},
	}
}

// chapterReadPercent reports how much of a chapter has been read (0-100).
func chapterReadPercent(c library.ChapterStatus) int {
	if c.Read {
		return 100
	}
	if c.ReadPages <= 0 {
		return 0
	}
	total := c.TotalPages
	if total <= 0 {
		total = c.Pages
	}
	if total <= 0 {
		return 0
	}
	pct := c.ReadPages * 100 / total
	if pct > 99 { // not marked complete: never claim 100%
		pct = 99
	}
	return pct
}

// chapterReadTip describes read progress as pages, e.g. "7/12 pages read".
func chapterReadTip(c library.ChapterStatus) string {
	total := c.TotalPages
	if total <= 0 {
		total = c.Pages
	}
	if total <= 0 {
		return ""
	}
	read := c.ReadPages
	if c.Read {
		read = total
	}
	return fmt.Sprintf("%d/%d pages read", read, total)
}

// chapterRowClass tints a chapter row by download state.
func chapterRowClass(c library.ChapterStatus) string {
	switch {
	case c.Downloaded:
		return "bg-success/5"
	case c.Failed:
		return "bg-error/5"
	default:
		return "opacity-50"
	}
}

func filterChapters(chs []library.ChapterStatus, q string) []library.ChapterStatus {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return chs
	}
	out := chs[:0]
	for _, c := range chs {
		if strings.Contains(strings.ToLower(c.Label), q) || strings.Contains(strings.ToLower(c.Title), q) {
			out = append(out, c)
		}
	}
	return out
}

func chapterTableParams(values url.Values) url.Values {
	out := url.Values{}
	if q := strings.TrimSpace(values.Get("q")); q != "" {
		out.Set("q", q)
	}
	if values.Get("dir") == "desc" {
		out.Set("dir", "desc")
	}
	if o := values.Get("order"); o != "" {
		out.Set("order", o)
	}
	return out
}

func mangaTitle(m catalog.Manga) string {
	for _, t := range []string{m.TitleEnglish, m.TitleRomaji, m.TitleNative} {
		if strings.TrimSpace(t) != "" {
			return t
		}
	}
	return "Untitled"
}

func jobLabel(typ string) string {
	switch typ {
	case jobs.TypeSyncAniList:
		return "AniList sync"
	case jobs.TypeCatalogRefresh:
		return "Catalog refresh"
	case jobs.TypeAttachVolumes:
		return "Attach volumes"
	case jobs.TypeRefreshTitle:
		return "Refresh chapters"
	case jobs.TypeScanDownloads:
		return "Scan downloads"
	case jobs.TypeDownloadMissing:
		return "Download missing"
	case jobs.TypeVerifySource:
		return "Verify source"
	case jobs.TypeMatchSources:
		return "Match sources"
	case jobs.TypeBackupUserData:
		return "Back up user data"
	case jobs.TypeDetectCrops:
		return "Detect page crops"
	}
	return typ
}

func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func percent(done, total int64) int64 {
	if total <= 0 {
		return 0
	}
	if done > total {
		done = total
	}
	return done * 100 / total
}
