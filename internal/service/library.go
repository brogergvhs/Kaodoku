package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/ui"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// LibraryService coordinates tracked titles and chapter discovery.
type LibraryService struct {
	repo     *library.Repository
	sources  *sources.Repository
	manga    func(ctx context.Context, catalogID int64) (catalog.Manga, error)
	syncOnce sync.Once
}

// RefreshResult describes one refreshed title.
type RefreshResult struct {
	Title library.Title
	Count int
}

// ScanResult describes filesystem verification for completed downloads.
type ScanResult struct {
	Checked int
	Missing int
}

// OpenLibrary opens the library database and applies migrations.
func OpenLibrary(ctx context.Context, dbPath string) (*LibraryService, func(), error) {
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := database.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, nil, err
	}

	svc := newLibraryService(db)
	if _, err := svc.ReconcileStartedDownloads(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if _, err := svc.ScanDownloads(ctx, nil, 0); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return svc, func() { _ = db.Close() }, nil
}

func newLibraryService(db *sql.DB) *LibraryService {
	return &LibraryService{repo: library.NewRepository(db), sources: sources.NewRepository(db)}
}

// ReconcileStartedDownloads marks interrupted downloads as failed.
func (s *LibraryService) ReconcileStartedDownloads(ctx context.Context) (int64, error) {
	return s.repo.ReconcileStartedDownloads(ctx)
}

// SetRefreshInterval sets a title's custom refresh cadence (empty = global).
func (s *LibraryService) SetRefreshInterval(ctx context.Context, id int64, interval string) error {
	return s.repo.SetRefreshInterval(ctx, id, interval)
}

// SetMonitored toggles monitoring for a tracked title.
func (s *LibraryService) TitleCover(ctx context.Context, id int64) ([]byte, string, error) {
	return s.repo.TitleCover(ctx, id)
}

func (s *LibraryService) SetTitleCover(ctx context.Context, id int64, blob []byte, mime string) error {
	return s.repo.SetTitleCover(ctx, id, blob, mime)
}

func (s *LibraryService) Volumes(ctx context.Context, titleID int64) ([]library.Volume, error) {
	return s.repo.Volumes(ctx, titleID)
}

func (s *LibraryService) GetVolume(ctx context.Context, id int64) (library.Volume, error) {
	return s.repo.GetVolume(ctx, id)
}

func (s *LibraryService) SetVolumeRead(ctx context.Context, id int64, read bool) error {
	return s.repo.SetVolumeRead(ctx, id, read)
}

func (s *LibraryService) Screens(ctx context.Context) ([]library.Screen, error) {
	return s.repo.Screens(ctx)
}

func (s *LibraryService) GetScreen(ctx context.Context, id int64) (library.Screen, error) {
	return s.repo.GetScreen(ctx, id)
}

func (s *LibraryService) SaveScreen(ctx context.Context, sc library.Screen) (int64, error) {
	return s.repo.SaveScreen(ctx, sc)
}

func (s *LibraryService) ReorderScreens(ctx context.Context, ids []int64) error {
	return s.repo.ReorderScreens(ctx, ids)
}

func (s *LibraryService) DeleteScreen(ctx context.Context, id int64) error {
	return s.repo.DeleteScreen(ctx, id)
}

func (s *LibraryService) LastReadAt(ctx context.Context) (map[int64]time.Time, error) {
	return s.repo.LastReadAt(ctx)
}

func (s *LibraryService) LatestArrivals(ctx context.Context) (map[int64]library.Arrival, error) {
	return s.repo.LatestArrivals(ctx)
}

func (s *LibraryService) MarkVolumePageRead(ctx context.Context, volumeID int64, page, totalPages int) (library.Volume, error) {
	return s.repo.MarkVolumePageRead(ctx, volumeID, page, totalPages)
}

func (s *LibraryService) VolumesReaderProgress(ctx context.Context, titleID int64) (library.TitleReadProgress, error) {
	title, err := s.repo.GetTitle(ctx, titleID)
	if err != nil {
		return library.TitleReadProgress{}, err
	}
	return s.repo.VolumesReaderProgress(ctx, title)
}

func (s *LibraryService) SetVolumeRangeRead(ctx context.Context, titleID int64, from, to float64, read bool) (int, error) {
	return s.repo.SetVolumeRangeRead(ctx, titleID, from, to, read)
}

func (s *LibraryService) VolumeThumb(ctx context.Context, id int64) ([]byte, string, error) {
	return s.repo.VolumeThumb(ctx, id)
}

func (s *LibraryService) VolumeCover(ctx context.Context, id int64) ([]byte, string, error) {
	return s.repo.VolumeCover(ctx, id)
}

func (s *LibraryService) SetVolumeCover(ctx context.Context, id int64, blob []byte, mime string) error {
	return s.repo.SetVolumeCover(ctx, id, blob, mime)
}

func (s *LibraryService) ToggleFavourite(ctx context.Context, titleID int64) (bool, error) {
	return s.repo.ToggleFavourite(ctx, titleID)
}

func (s *LibraryService) SetFavourite(ctx context.Context, userID, titleID int64) error {
	return s.repo.SetFavourite(ctx, userID, titleID)
}

func (s *LibraryService) SetLanguageMode(ctx context.Context, id int64, mode string) error {
	switch mode {
	case "preferred", "all", "off":
	default:
		return fmt.Errorf("unknown language mode %q", mode)
	}
	return s.repo.SetLanguageMode(ctx, id, mode)
}

func (s *LibraryService) SetMonitored(ctx context.Context, id int64, monitored bool) error {
	return s.repo.SetMonitored(ctx, id, monitored)
}

// AddTitle tracks a source URL.
func (s *LibraryService) AddTitle(ctx context.Context, params library.AddTitleParams) (library.Title, error) {
	if params.SourceID == "" {
		if src, ok := s.sourceForURL(ctx, params.SourceURL); ok {
			params.SourceID = src.ID
		}
	}
	return s.repo.AddTitle(ctx, params)
}

// ListTitles returns tracked titles.
func (s *LibraryService) ListTitles(ctx context.Context) ([]library.Title, error) {
	return s.repo.ListTitles(ctx)
}

// ResetFailedDownloads clears the attempt cap on a title's failed downloads.
func (s *LibraryService) ResetFailedDownloads(ctx context.Context, titleID int64) error {
	return s.repo.ResetFailedDownloads(ctx, titleID)
}

// TitlesByProvider maps a catalog provider's manga IDs to tracked title IDs.
func (s *LibraryService) TitlesByProvider(ctx context.Context, provider string) (map[string]int64, error) {
	return s.repo.TitlesByProvider(ctx, provider)
}

// ListTitleSources returns all sources linked to a title.
func (s *LibraryService) ListTitleSources(ctx context.Context, titleID int64) ([]library.LinkedSource, error) {
	return s.repo.ListTitleSources(ctx, titleID)
}

// UnlinkSource removes a linked source from a title.
func (s *LibraryService) UnlinkSource(ctx context.Context, titleID int64, url string) error {
	return s.repo.UnlinkSource(ctx, titleID, url)
}

// ListChapters returns all discovered chapters for a title with download state.
func (s *LibraryService) ListChapters(ctx context.Context, titleID int64) ([]library.ChapterStatus, error) {
	return s.repo.ListChapters(ctx, titleID)
}

// ReaderProgress returns downloaded chapters and read state for a title.
func (s *LibraryService) ReaderProgress(ctx context.Context, titleID int64) (library.TitleReadProgress, error) {
	return s.repo.ReaderProgress(ctx, titleID)
}

// ChapterReadStatus returns reader/download state for one chapter.
func (s *LibraryService) ChapterReadStatus(ctx context.Context, chapterID int64) (library.ChapterReadStatus, error) {
	return s.repo.GetChapterReadStatus(ctx, chapterID)
}

// MarkPageRead records one completed page for reader resume/progress.
func (s *LibraryService) MarkPageRead(ctx context.Context, chapterID int64, page, totalPages int) (library.ChapterReadStatus, error) {
	return s.repo.MarkPageRead(ctx, chapterID, page, totalPages)
}

// MarkPageReadAt marks a page read at an explicit time (offline replay).
func (s *LibraryService) MarkPageReadAt(ctx context.Context, chapterID int64, page, totalPages int, readAt string) (library.ChapterReadStatus, error) {
	return s.repo.MarkPageReadAt(ctx, chapterID, page, totalPages, readAt)
}

// ChaptersReadSince returns chapter progress touched after since.
func (s *LibraryService) ChaptersReadSince(ctx context.Context, since string) ([]library.ChapterReadStatus, error) {
	return s.repo.ChaptersReadSince(ctx, since)
}

// ReadProgressIDs returns all chapter/volume ids with progress rows.
func (s *LibraryService) ReadProgressIDs(ctx context.Context) ([]int64, []int64, error) {
	return s.repo.ReadProgressIDs(ctx)
}

// VolumesReadSince returns volume progress touched after since.
func (s *LibraryService) VolumesReadSince(ctx context.Context, since string) ([]library.Volume, error) {
	return s.repo.VolumesReadSince(ctx, since)
}

// MarkChapterRead records a completed chapter.
func (s *LibraryService) MarkChapterRead(ctx context.Context, chapterID int64) (library.ChapterReadStatus, error) {
	return s.repo.MarkChapterRead(ctx, chapterID)
}

// MarkChapterUnread clears read progress for a chapter.
func (s *LibraryService) MarkChapterUnread(ctx context.Context, chapterID int64) (library.ChapterReadStatus, error) {
	return s.repo.MarkChapterUnread(ctx, chapterID)
}

// RemoveChapterDownload deletes a chapter's downloaded file from disk and its
// download record, so the chapter shows as missing again.
func (s *LibraryService) RemoveChapterDownload(ctx context.Context, chapterID int64) error {
	file, err := s.repo.DeleteDownload(ctx, chapterID)
	if err != nil {
		return err
	}
	if file != "" {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("download record removed, but deleting %s failed: %w", file, err)
		}
	}
	return nil
}

// RenameChapter updates a chapter's descriptive title.
func (s *LibraryService) RenameChapter(ctx context.Context, chapterID int64, title string) error {
	return s.repo.RenameChapter(ctx, chapterID, title)
}

// ResetChapterTitle restores one manually renamed chapter's scraped name.
func (s *LibraryService) ResetChapterTitle(ctx context.Context, chapterID int64) error {
	return s.repo.ResetChapterTitle(ctx, chapterID)
}

// BulkRenameChapters applies manual names and volume assignments atomically.
func (s *LibraryService) BulkRenameChapters(ctx context.Context, titleID int64, items []library.ChapterRename) error {
	return s.repo.BulkRenameChapters(ctx, titleID, items)
}

// ResetChapterTitles restores scraped names for the whole title (blank range)
// or an inclusive label/number range.
func (s *LibraryService) ResetChapterTitles(ctx context.Context, titleID int64, from, to string) (int, error) {
	return s.repo.ResetChapterTitles(ctx, titleID, from, to)
}

// TitleReadStatuses lists all discovered chapters with download + read state.
func (s *LibraryService) TitleReadStatuses(ctx context.Context, titleID int64) ([]library.ChapterReadStatus, error) {
	return s.repo.TitleReadStatuses(ctx, titleID)
}

// TitleOwners maps title id to the user that added it.
func (s *LibraryService) TitleOwners(ctx context.Context) (map[int64]int64, error) {
	return s.repo.TitleOwners(ctx)
}

// MarkChapterRangeRead marks downloaded chapters in a title range read.
// MarkChaptersReadThrough marks chapters up to a number read (AniList pull).
func (s *LibraryService) MarkChaptersReadThrough(ctx context.Context, titleID int64, maxNumber int) (int, error) {
	return s.repo.MarkChaptersReadThrough(ctx, titleID, maxNumber)
}

func (s *LibraryService) MarkChapterRangeRead(ctx context.Context, titleID int64, from, to string) (int, error) {
	return s.repo.MarkChapterRangeRead(ctx, titleID, from, to)
}

// MarkChapterRangeUnread clears read progress in a title range.
func (s *LibraryService) MarkChapterRangeUnread(ctx context.Context, titleID int64, from, to string) (int, error) {
	return s.repo.MarkChapterRangeUnread(ctx, titleID, from, to)
}

// MissingChapters returns a title and its missing chapters.
func (s *LibraryService) MissingChapters(ctx context.Context, id int64) (library.Title, []library.Chapter, error) {
	title, err := s.repo.GetTitle(ctx, id)
	if err != nil {
		return library.Title{}, nil, err
	}
	missing, err := s.repo.ListMissingChapters(ctx, id)
	if err != nil {
		return library.Title{}, nil, err
	}

	return title, missing, nil
}

// RemoveTitle removes a tracked title and returns the removed title.
func (s *LibraryService) RemoveTitle(ctx context.Context, id int64) (library.Title, error) {
	title, err := s.repo.GetTitle(ctx, id)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.repo.RemoveTitle(ctx, id); err != nil {
		return library.Title{}, err
	}

	return title, nil
}

// GetTitle returns one tracked title.
func (s *LibraryService) GetTitle(ctx context.Context, id int64) (library.Title, error) {
	return s.repo.GetTitle(ctx, id)
}

// RefreshTitle refreshes discovered chapters for one title.
func (s *LibraryService) RefreshTitle(
	ctx context.Context,
	cfg *config.Config,
	logSvc ui.Log,
	title library.Title,
) (RefreshResult, error) {
	logSvc = logSvc.With("title_id", title.ID, "title", title.DisplayTitle)
	if !strings.HasPrefix(title.SourceURL, "http") {
		return RefreshResult{}, fmt.Errorf("title %q has no linked source to refresh from", title.DisplayTitle)
	}
	downloadSvc, err := s.downloadServiceForTitle(ctx, cfg, logSvc, nil, title)
	if err != nil {
		return RefreshResult{}, err
	}

	chapters, finalURL, languageGap, err := downloadSvc.FetchChapters(ctx, title.SourceURL)
	if err != nil {
		return RefreshResult{}, err
	}
	if int64(languageGap) != title.LanguageGap {
		if err := s.repo.SetLanguageGap(ctx, title.ID, languageGap); err != nil {
			return RefreshResult{}, err
		}
	}
	if finalURL != "" && finalURL != title.SourceURL && strings.HasPrefix(finalURL, "http") {
		if err := s.repo.UpdateSourceURL(ctx, title.ID, title.SourceURL, finalURL); err != nil {
			logSvc.Infof("Could not persist moved source URL for %q: %v\n", title.DisplayTitle, err)
		} else {
			logSvc.Infof("Source URL for %q moved: %s -> %s\n", title.DisplayTitle, title.SourceURL, finalURL)
			title.SourceURL = finalURL
		}
	}
	if len(chapters) == 0 {
		return RefreshResult{}, fmt.Errorf("no chapters found at %s — the site may need FlareSolverr, or the source URL is wrong", title.SourceURL)
	}
	count, err := s.repo.UpsertChapters(ctx, title.ID, chapters)
	if err != nil {
		return RefreshResult{}, err
	}

	return RefreshResult{Title: title, Count: count}, nil
}

// RefreshMonitored refreshes all monitored titles.
func (s *LibraryService) RefreshMonitored(
	ctx context.Context,
	cfg *config.Config,
	logSvc ui.Log,
) ([]RefreshResult, error) {
	titles, err := s.repo.ListTitles(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]RefreshResult, 0, len(titles))
	var errs []error
	for _, title := range titles {
		if !title.Monitored {
			continue
		}
		result, err := s.RefreshTitle(ctx, cfg, logSvc, title)
		if err != nil {
			errs = append(errs, fmt.Errorf("refresh %s: %w", title.DisplayTitle, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		results = append(results, result)
	}

	return results, errors.Join(errs...)
}

// ScanDownloads verifies completed download files still exist.
func (s *LibraryService) ScanDownloads(ctx context.Context, cfg *config.Config, titleID int64) (ScanResult, error) {
	downloads, err := s.repo.ListScannableDownloads(ctx, titleID)
	if err != nil {
		return ScanResult{}, err
	}

	type titleIndex struct {
		byName  map[string]string // filename -> full path
		byLabel map[string]string // parsed chapter label -> full path
	}
	titleFiles := map[int64]*titleIndex{}
	indexFor := func(titleID int64) *titleIndex {
		if cfg == nil {
			return &titleIndex{}
		}
		if idx, ok := titleFiles[titleID]; ok {
			return idx
		}
		idx := &titleIndex{byName: map[string]string{}, byLabel: map[string]string{}}
		titleFiles[titleID] = idx
		if title, err := s.repo.GetTitle(ctx, titleID); err == nil {
			if dir, err := s.TitleFilesDir(cfg, title); err == nil {
				for _, sub := range []string{dir, filepath.Join(dir, chaptersSubdir)} {
					entries, err := os.ReadDir(sub)
					if err != nil {
						continue
					}
					for _, e := range entries {
						if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".cbz") {
							continue
						}
						full := filepath.Join(sub, e.Name())
						idx.byName[e.Name()] = full
						if label, _ := parseChapterFile(e.Name()); label != "" {
							if _, dup := idx.byLabel[label]; !dup {
								idx.byLabel[label] = full
							}
						}
					}
				}
			}
		}
		return idx
	}
	resolve := func(d library.CompletedDownload) string {
		idx := indexFor(d.TitleID)
		if base := filepath.Base(d.OutputFile); d.OutputFile != "" {
			if hit := idx.byName[base]; hit != "" {
				return hit
			}
		}
		// The path may be gone entirely (older failure handling erased it);
		// fall back to matching the chapter label parsed from filenames.
		return idx.byLabel[d.Label]
	}

	type verdict struct {
		download library.CompletedDownload
		file     string
		size     int64
	}
	var found []verdict
	var lost []library.CompletedDownload
	var result ScanResult
	for _, download := range downloads {
		result.Checked++
		file := download.OutputFile
		var info os.FileInfo
		err := error(os.ErrNotExist)
		if file != "" {
			info, err = os.Stat(file)
		}
		if os.IsNotExist(err) && file != "" {
			alt := filepath.Join(filepath.Dir(file), chaptersSubdir, filepath.Base(file))
			if altInfo, altErr := os.Stat(alt); altErr == nil {
				file, info, err = alt, altInfo, nil
			}
		}
		if os.IsNotExist(err) {
			if hit := resolve(download); hit != "" {
				if hInfo, hErr := os.Stat(hit); hErr == nil {
					file, info, err = hit, hInfo, nil
				}
			}
		}
		if err == nil {
			found = append(found, verdict{download: download, file: file, size: info.Size()})
			continue
		} else if !os.IsNotExist(err) {
			return result, fmt.Errorf("check %s: %w", file, err)
		}
		lost = append(lost, download)
	}

	if len(found) == 0 && len(lost) >= 5 {
		sample := lost[0]
		dirs := "(no config: current-dir resolution skipped)"
		if cfg != nil {
			if title, err := s.repo.GetTitle(ctx, sample.TitleID); err == nil {
				if dir, err := s.TitleFilesDir(cfg, title); err == nil {
					dirs = dir + " and " + filepath.Join(dir, chaptersSubdir)
				} else {
					dirs = "unresolvable title dir: " + err.Error()
				}
			}
		}
		return result, fmt.Errorf("scan aborted: none of %d files found — nothing was marked failed. Recorded path example: %s; also searched %s. Check the download directory setting/mount", len(lost), sample.OutputFile, dirs)
	}

	for _, v := range found {
		if err := s.repo.MarkDownloadCompleted(ctx, v.download.ChapterID, v.file, v.size, cbzPageCount(v.file)); err != nil {
			return result, err
		}
	}
	for _, download := range lost {
		result.Missing++
		if err := s.repo.MarkDownloadFailed(ctx, download.ChapterID, fmt.Errorf("output file missing: %s", download.OutputFile)); err != nil {
			return result, err
		}
	}
	return result, nil
}

// DownloadMissing downloads every missing chapter for a title.
func (s *LibraryService) DownloadMissing(
	ctx context.Context,
	cfg *config.Config,
	logSvc ui.Log,
	titleID int64,
	progress ProgressManager,
) ([]ChapterDownloadResult, error) {
	title, missing, err := s.MissingChapters(ctx, titleID)
	if err != nil {
		return nil, err
	}
	if title.LanguageMode == "off" {
		logSvc.Infof("Downloads for %q are turned off (language choice).\n", title.DisplayTitle)
		return nil, nil
	}

	cfg, err = configForTitle(cfg, title)
	if err != nil {
		return nil, err
	}
	if has, err := s.repo.HasVolumes(ctx, titleID); err == nil && has {
		chaptersDir, _, err := s.EnsureVolumeChapterSplit(ctx, cfg, title)
		if err != nil {
			return nil, err
		}
		cfg.Output = chaptersDir
	}
	downloadSvc, err := s.downloadServiceForTitle(ctx, cfg, logSvc, progress, title)
	if err != nil {
		return nil, err
	}
	const maxConsecutiveFailures = 10
	consecutive := 0
	results := make([]ChapterDownloadResult, 0, len(missing))
	var errs []error
	for _, chapter := range missing {
		result, err := s.downloadChapter(ctx, downloadSvc, chapter)
		if err != nil {
			errs = append(errs, fmt.Errorf("chapter %s: %w", chapter.Label, err))
			if ctx.Err() != nil {
				break
			}
			if consecutive++; consecutive >= maxConsecutiveFailures {
				errs = append(errs, fmt.Errorf("stopping after %d consecutive chapter failures", consecutive))
				break
			}
			continue
		}
		consecutive = 0
		results = append(results, result)
	}

	return results, errors.Join(errs...)
}

// DownloadMonitoredMissing downloads missing chapters for every monitored title.
func (s *LibraryService) DownloadMonitoredMissing(
	ctx context.Context,
	cfg *config.Config,
	logSvc ui.Log,
) ([]ChapterDownloadResult, error) {
	titles, err := s.repo.ListTitles(ctx)
	if err != nil {
		return nil, err
	}

	var results []ChapterDownloadResult
	var errs []error
	for _, title := range titles {
		if !title.Monitored {
			continue
		}
		titleResults, err := s.DownloadMissing(ctx, cfg, logSvc, title.ID, nil)
		results = append(results, titleResults...)
		if err != nil {
			errs = append(errs, fmt.Errorf("download missing for %s: %w", title.DisplayTitle, err))
			if ctx.Err() != nil {
				break
			}
		}
	}

	return results, errors.Join(errs...)
}

// DownloadChapterLabel downloads one discovered chapter by label.
func (s *LibraryService) DownloadChapterLabel(
	ctx context.Context,
	cfg *config.Config,
	logSvc ui.Log,
	titleID int64,
	label string,
) (ChapterDownloadResult, error) {
	title, err := s.repo.GetTitle(ctx, titleID)
	if err != nil {
		return ChapterDownloadResult{}, err
	}
	chapter, err := s.repo.GetChapterByLabel(ctx, titleID, label)
	if err != nil {
		return ChapterDownloadResult{}, err
	}

	titleCfg, err := configForTitle(cfg, title)
	if err != nil {
		return ChapterDownloadResult{}, err
	}
	downloadSvc, err := s.downloadServiceForTitle(ctx, titleCfg, logSvc, nil, title)
	if err != nil {
		return ChapterDownloadResult{}, err
	}
	return s.downloadChapter(ctx, downloadSvc, chapter)
}

func (s *LibraryService) downloadChapter(
	ctx context.Context,
	downloadSvc *DownloadService,
	chapter library.Chapter,
) (ChapterDownloadResult, error) {
	if err := s.repo.MarkDownloadStarted(ctx, chapter.ID); err != nil {
		return ChapterDownloadResult{}, err
	}

	// Outcomes must be persisted even when ctx expired mid-download, or the
	// row strands as 'started' and startup reconciliation burns an attempt.
	markCtx := context.WithoutCancel(ctx)
	result, err := downloadSvc.DownloadChapter(ctx, serviceChapter(chapter))
	if err != nil {
		if markErr := s.repo.MarkDownloadFailed(markCtx, chapter.ID, err); markErr != nil {
			return ChapterDownloadResult{}, fmt.Errorf("%w; mark download failed: %v", err, markErr)
		}
		return ChapterDownloadResult{}, err
	}
	if err := s.repo.MarkDownloadCompleted(markCtx, chapter.ID, result.OutputFile, result.Bytes, result.Images); err != nil {
		return ChapterDownloadResult{}, err
	}

	return result, nil
}

func (s *LibraryService) downloadServiceForTitle(
	ctx context.Context,
	cfg *config.Config,
	logSvc ui.Log,
	progress ProgressManager,
	title library.Title,
) (*DownloadService, error) {
	svc, err := func() (*DownloadService, error) {
		if src, ok := s.sourceForTitle(ctx, title); ok {
			next := ConfigForSource(*cfg, src, SourceConfigOptions{})
			next.LanguageMode = title.LanguageMode
			return NewSourceDownloadService(&next, logSvc, progress, src.Scraper)
		}
		return NewDefaultDownloadService(cfg, logSvc, progress)
	}()
	if err == nil {
		svc.SetComicInfo(s.comicInfoTemplate(ctx, title))
	}
	return svc, err
}

// SetMangaLookup wires catalog metadata access for ComicInfo templates.
func (s *LibraryService) SetMangaLookup(fn func(ctx context.Context, catalogID int64) (catalog.Manga, error)) {
	s.manga = fn
}

// comicInfoTemplate builds the series-level ComicInfo for a title, or nil
// when no catalog entry is linked.
func (s *LibraryService) comicInfoTemplate(ctx context.Context, title library.Title) *util.ComicInfo {
	ci := util.ComicInfo{Series: title.DisplayTitle, Web: title.SourceURL, Manga: "Yes"}
	if s.manga != nil && title.CatalogMangaID != nil {
		if m, err := s.manga(ctx, *title.CatalogMangaID); err == nil {
			ci.Summary = util.PlainSnippet(m.Description, 1500)
			ci.Writer = strings.Join(m.Authors, ", ")
			ci.Genre = strings.Join(m.Genres, ", ")
			ci.Tags = strings.Join(m.Tags, ", ")
			ci.Year = m.Year
			if m.Chapters != nil {
				ci.Count = *m.Chapters
			}
			if m.IsAdult {
				ci.AgeRating = "Adults Only 18+"
			}
		}
	}
	return &ci
}

func (s *LibraryService) sourceForTitle(ctx context.Context, title library.Title) (sources.Source, bool) {
	if strings.TrimSpace(title.SourceID) != "" {
		if list, err := s.listSources(ctx); err == nil {
			for _, src := range list {
				if src.ID == title.SourceID && src.Enabled {
					return src, true
				}
			}
		}
	}
	return s.sourceForURL(ctx, title.SourceURL)
}

func (s *LibraryService) sourceForURL(ctx context.Context, target string) (sources.Source, bool) {
	list, err := s.listSources(ctx)
	if err != nil {
		return sources.Source{}, false
	}
	return MatchSourceForURL(list, target)
}

func (s *LibraryService) listSources(ctx context.Context) ([]sources.Source, error) {
	profiles, err := sources.BuiltInProfiles()
	if err != nil {
		return nil, err
	}
	if s.sources == nil {
		out := make([]sources.Source, 0, len(profiles))
		for _, profile := range profiles {
			out = append(out, sourceFromProfile(profile, sources.OriginBuiltin))
		}
		return out, nil
	}
	// Built-in profiles only change with the binary; one sync per process.
	var syncErr error
	s.syncOnce.Do(func() {
		syncErr = s.sources.Sync(ctx, profiles, sources.OriginBuiltin)
	})
	if syncErr != nil {
		return nil, syncErr
	}
	return s.sources.List(ctx)
}

func serviceChapter(chapter library.Chapter) chapters.Chapter {
	return chapters.Chapter{Chapter: providers.Chapter{
		URL:        chapter.URL,
		Title:      chapter.Title,
		NumMain:    chapter.NumberMain,
		SuffixType: chapter.SuffixType,
		SuffixNum:  chapter.SuffixNum,
		Label:      chapter.Label,
	}}
}

func configForTitle(cfg *config.Config, title library.Title) (*config.Config, error) {
	next := *cfg
	root, err := filepath.Abs(next.DownloadDir)
	if err != nil {
		return nil, fmt.Errorf("resolve download root: %w", err)
	}
	if title.OutputPath != "" {
		output := title.OutputPath
		if !filepath.IsAbs(output) {
			output = filepath.Join(root, output)
		}
		output, err = filepath.Abs(output)
		if err != nil {
			return nil, fmt.Errorf("resolve output path: %w", err)
		}
		if _, ok := withinRoot(root, output); !ok {
			return nil, fmt.Errorf("output path %q is outside download root %q", title.OutputPath, next.DownloadDir)
		}
		next.Output = output
	} else {
		next.Output = filepath.Join(root, titleOutputDir(title))
	}
	return &next, nil
}

// TitleFilesDir resolves the folder holding a title's downloads, strictly
// inside the download root (never the root itself).
func (s *LibraryService) TitleFilesDir(cfg *config.Config, title library.Title) (string, error) {
	titleCfg, err := configForTitle(cfg, title)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(titleCfg.DownloadDir)
	if err != nil {
		return "", err
	}
	dir := titleCfg.Output
	if rel, ok := withinRoot(root, dir); !ok || rel == "." {
		return "", fmt.Errorf("refusing to touch %q: not strictly inside the download root", dir)
	}
	return dir, nil
}

func withinRoot(root, dir string) (rel string, ok bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return rel, false
	}
	return rel, true
}

func titleOutputDir(title library.Title) string {
	name := strings.TrimSpace(title.DisplayTitle)
	if name == "" {
		return fmt.Sprintf("title_%d", title.ID)
	}

	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}

	out := strings.Trim(b.String(), "_")
	if out == "" {
		return fmt.Sprintf("title_%d", title.ID)
	}

	return out
}
