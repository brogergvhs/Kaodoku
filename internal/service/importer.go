package service

import (
	"archive/zip"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// ImportCandidate is a download-dir folder not yet tracked as a title.
type ImportCandidate struct {
	Folder       string `json:"folder"`
	ChapterFiles int    `json:"chapter_files"`
}

// ExploreDownloads lists direct subfolders of root that hold .cbz files and
// are not already tracked.
func (s *CatalogService) ExploreDownloads(ctx context.Context, root string) ([]ImportCandidate, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve download dir: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read download dir: %w", err)
	}
	titles, err := s.library.TitleBasics(ctx, library.TitleQuery{})
	if err != nil {
		return nil, err
	}
	tracked := make(map[string]bool, len(titles))
	for _, t := range titles {
		tracked[trackedDir(t)] = true
	}

	var out []ImportCandidate
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), "_tmp") || tracked[e.Name()] {
			continue
		}
		if n := len(cbzFiles(filepath.Join(root, e.Name()))); n > 0 {
			out = append(out, ImportCandidate{Folder: e.Name(), ChapterFiles: n})
		}
	}
	return out, nil
}

// ImportFolder tracks an existing folder as a title linked to a catalog entry
// and records its .cbz files as completed downloads.
func (s *CatalogService) ImportFolder(ctx context.Context, root, folder string, p catalog.Provider, id string) (library.Title, error) {
	if folder == "" || folder == "." || folder == ".." || strings.ContainsAny(folder, `/\`) {
		return library.Title{}, fmt.Errorf("invalid folder %q", folder)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return library.Title{}, err
	}
	dir := filepath.Join(root, folder)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return library.Title{}, fmt.Errorf("folder %q not found", folder)
	}

	manga, err := s.fetch(ctx, p, id)
	if err != nil {
		return library.Title{}, err
	}

	title, err := s.library.AddTitle(ctx, library.AddTitleParams{
		CatalogMangaID:  &manga.ID,
		SourceURL:       localURL(folder),
		DisplayTitle:    displayMangaTitle(manga),
		OutputPath:      folder,
		Monitored:       defaultMonitored(manga),
		RefreshInterval: "",
	})
	if err != nil {
		return library.Title{}, err
	}

	// One chapter per label (the first file wins); the chapter list must
	// dedupe by label since (title_id, label) is unique.
	type parsedFile struct {
		file  string
		num   int
		title string
	}
	byLabel := map[string]parsedFile{}
	var labels []string
	for _, f := range cbzFiles(dir) {
		label, num, title := importedChapterMeta(dir, f)
		if _, seen := byLabel[label]; seen {
			if fnLabel, fnNum := parseChapterFile(f); byLabel[fnLabel].file == "" {
				label, num = fnLabel, fnNum
			} else {
				continue
			}
		}
		byLabel[label] = parsedFile{file: f, num: num, title: title}
		labels = append(labels, label)
	}

	discovered := make([]chapters.Chapter, 0, len(labels))
	for _, label := range labels {
		p := byLabel[label]
		discovered = append(discovered, chapters.Chapter{Chapter: providers.Chapter{
			Label:   label,
			NumMain: p.num,
			Title:   p.title,
			URL:     localURL(folder + "/" + p.file),
		}})
	}
	if _, err := s.library.UpsertChapters(ctx, title.ID, discovered); err != nil {
		return library.Title{}, err
	}
	for _, label := range labels {
		ch, err := s.library.GetChapterByLabel(ctx, title.ID, label)
		if err != nil {
			continue
		}
		path := filepath.Join(dir, byLabel[label].file)
		var size int64
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
		if err := s.library.MarkDownloadCompleted(ctx, ch.ID, path, size, cbzPageCount(path)); err != nil {
			return library.Title{}, err
		}
	}
	return s.library.GetTitle(ctx, title.ID)
}

// AddCatalogTitle adds a catalog manga to the library as a source-less title
// (deduped per manga) so its sources can be linked later.
func (s *CatalogService) AddCatalogTitle(ctx context.Context, p catalog.Provider, id string) (library.Title, error) {
	manga, err := s.fetch(ctx, p, id)
	if err != nil {
		return library.Title{}, err
	}
	if existing, ok, err := s.library.FindByCatalog(ctx, manga.ID); err != nil {
		return library.Title{}, err
	} else if ok {
		return existing, nil
	}
	return s.library.AddTitle(ctx, library.AddTitleParams{
		CatalogMangaID:  &manga.ID,
		SourceURL:       fmt.Sprintf("pending:%d", manga.ID),
		DisplayTitle:    displayMangaTitle(manga),
		Monitored:       defaultMonitored(manga),
		RefreshInterval: "",
	})
}

func defaultMonitored(manga catalog.Manga) bool {
	switch strings.ToLower(strings.TrimSpace(manga.Status)) {
	case "finished", "completed", "complete":
		return false
	default:
		return true
	}
}

// GetManga returns stored catalog metadata.
func (s *CatalogService) GetManga(ctx context.Context, catalogID int64) (catalog.Manga, error) {
	return s.repo.GetManga(ctx, catalogID)
}

// LinkTitleSource points an imported title at a real source so future refreshes
// and downloads work.
func (s *CatalogService) LinkTitleSource(ctx context.Context, titleID, matchID int64) (library.Title, error) {
	match, err := s.repo.GetMatch(ctx, matchID)
	if err != nil {
		return library.Title{}, err
	}
	if err := s.library.LinkSource(ctx, titleID, match.SourceURL, match.SourceID); err != nil {
		return library.Title{}, err
	}
	return s.library.GetTitle(ctx, titleID)
}

// LinkTitleURL points a title at a specific chapter-list URL, for single-manga
// or custom sites that have no searchable catalog.
func (s *CatalogService) LinkTitleURL(ctx context.Context, titleID int64, rawURL, sourceID string) (library.Title, error) {
	if err := s.library.LinkSource(ctx, titleID, rawURL, sourceID); err != nil {
		return library.Title{}, err
	}
	return s.library.GetTitle(ctx, titleID)
}

// trackedDir is the folder name a tracked title downloads into.
func trackedDir(t library.Title) string {
	if t.OutputPath != "" {
		return filepath.Base(t.OutputPath)
	}
	return titleOutputDir(t)
}

func localURL(s string) string {
	return "local:" + url.PathEscape(s)
}

// cbzPageCount counts image entries in a CBZ archive.
func cbzPageCount(path string) int {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return 0
	}
	defer zr.Close()
	n := 0
	for _, f := range zr.File {
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif":
			n++
		}
	}
	return n
}

func cbzFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".cbz") {
			out = append(out, e.Name())
		}
	}
	return out
}

var (
	// A leading number keyed to the chapter, e.g. "12: Title", "12 - Title",
	// "33 Kyoto ... part 0", "012.cbz". Ripped collections almost always lead
	// with the chapter number, so a separator (space or punctuation) or the end
	// of the name after it is enough — the leading number wins over any trailing
	// "part N" (which is a sub-part, not a chapter).
	reFileLead    = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)(?:[\s:.)\]_-]|$)`)
	reFileChapter = regexp.MustCompile(`(?i)(?:chapter|episode|ch|ep|c)[\s._-]*([0-9]+(?:\.[0-9]+)?)`)
	reFileNumber  = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
)

// importedChapterMeta derives a chapter's label, number, and title from the
// file, preferring embedded ComicInfo.xml over filename parsing.
func importedChapterMeta(dir, f string) (string, int, string) {
	label, num := parseChapterFile(f)
	title := strings.TrimSuffix(f, filepath.Ext(f))
	if ci, ok := util.ReadComicInfo(filepath.Join(dir, f)); ok {
		if main, _, _, lbl, numOK := providers.ParseChapterNumber(ci.Number); numOK {
			label, num = lbl, main
		} else if strings.TrimSpace(ci.Number) != "" {
			label = strings.TrimSpace(ci.Number)
		}
		if strings.TrimSpace(ci.Title) != "" {
			title = strings.TrimSpace(ci.Title)
		}
	}
	return label, num, title
}

// parseChapterFile derives a chapter label and main number from a .cbz name.
// The label is normalized (leading zeros stripped) to match scraper output so
// imported chapters merge with a later source refresh.
func parseChapterFile(name string) (label string, num int) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	raw := ""
	switch {
	case reFileLead.MatchString(stem):
		raw = reFileLead.FindStringSubmatch(stem)[1] // "12: Title" -> 12
	case reFileChapter.MatchString(stem):
		raw = reFileChapter.FindStringSubmatch(stem)[1] // "Vol 2 Ch 7" -> 7
	default:
		if nums := reFileNumber.FindAllString(stem, -1); len(nums) > 0 {
			raw = nums[len(nums)-1] // last standalone number
		}
	}
	if raw == "" {
		return strings.TrimSpace(stem), 0
	}
	mainStr, frac, hasFrac := strings.Cut(raw, ".")
	num, _ = strconv.Atoi(mainStr)
	label = strconv.Itoa(num) // "001" -> "1"
	if hasFrac {
		label += "." + frac // keep fractional part verbatim: "12.05"
	}
	return label, num
}
