package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/library"
)

const (
	chaptersSubdir = "Chapters"
	volumesSubdir  = "Volumes"
)

// ImportVolumesFolder tracks a folder of volume .cbz files as a new title.
// Volumes are disk-only, so no source search is started for them.
func (s *CatalogService) ImportVolumesFolder(ctx context.Context, root, folder string, p catalog.Provider, id string) (library.Title, error) {
	dir, err := importDir(root, folder)
	if err != nil {
		return library.Title{}, err
	}
	manga, err := s.fetch(ctx, p, id)
	if err != nil {
		return library.Title{}, err
	}
	title, err := s.library.AddTitle(ctx, library.AddTitleParams{
		CatalogMangaID: &manga.ID,
		SourceURL:      localURL(folder),
		DisplayTitle:   displayMangaTitle(manga),
		OutputPath:     folder,
		Monitored:      false, // nothing to monitor without a chapter source
	})
	if err != nil {
		return library.Title{}, err
	}
	if _, err := s.library.SyncVolumeFiles(ctx, title.ID, dir, cbzPageCount); err != nil {
		return library.Title{}, err
	}
	if err := generateVolumeThumbs(ctx, s.library, title.ID); err != nil {
		return library.Title{}, err
	}
	return s.library.GetTitle(ctx, title.ID)
}

func importDir(root, folder string) (string, error) {
	if folder == "" || folder == "." || folder == ".." || containsPathSep(folder) {
		return "", fmt.Errorf("invalid folder %q", folder)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, folder)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("folder %q not found", folder)
	}
	return dir, nil
}

func containsPathSep(s string) bool {
	for _, r := range s {
		if r == '/' || r == '\\' {
			return true
		}
	}
	return false
}

// EnsureVolumeChapterSplit moves a title that holds both volumes and chapters
// into the Chapters/ + Volumes/ layout, updating stored paths. Idempotent;
// returns the two subdirectories.
func (s *LibraryService) EnsureVolumeChapterSplit(ctx context.Context, cfg *config.Config, title library.Title) (chaptersDir, volumesDir string, err error) {
	base, err := s.TitleFilesDir(cfg, title)
	if err != nil {
		return "", "", err
	}
	chaptersDir = filepath.Join(base, chaptersSubdir)
	volumesDir = filepath.Join(base, volumesSubdir)
	if err := os.MkdirAll(chaptersDir, 0o755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(volumesDir, 0o755); err != nil {
		return "", "", err
	}

	downloads, err := s.repo.ListCompletedDownloads(ctx, title.ID)
	if err != nil {
		return "", "", err
	}
	for _, d := range downloads {
		if filepath.Dir(d.OutputFile) != base {
			continue
		}
		next := filepath.Join(chaptersDir, filepath.Base(d.OutputFile))
		if err := moveFile(d.OutputFile, next); err != nil && !os.IsNotExist(err) {
			return "", "", fmt.Errorf("move %s: %w", d.OutputFile, err)
		}
		if err := s.repo.UpdateDownloadFile(ctx, d.ChapterID, next); err != nil {
			return "", "", err
		}
	}

	vols, err := s.repo.Volumes(ctx, title.ID)
	if err != nil {
		return "", "", err
	}
	for _, v := range vols {
		if filepath.Dir(v.File) != base {
			continue
		}
		if err := moveFile(v.File, filepath.Join(volumesDir, filepath.Base(v.File))); err != nil && !os.IsNotExist(err) {
			return "", "", fmt.Errorf("move %s: %w", v.File, err)
		}
	}
	if err := s.repo.MoveVolumeFiles(ctx, title.ID, base, volumesDir); err != nil {
		return "", "", err
	}
	return chaptersDir, volumesDir, nil
}

// AttachVolumesFolder moves the .cbz files of an untracked download folder
// into a tracked title's Volumes/ directory and records them as volumes.
func (s *LibraryService) AttachVolumesFolder(ctx context.Context, cfg *config.Config, title library.Title, folder string) error {
	srcDir, err := importDir(cfg.DownloadDir, folder)
	if err != nil {
		return err
	}
	_, volumesDir, err := s.EnsureVolumeChapterSplit(ctx, cfg, title)
	if err != nil {
		return err
	}
	if srcDir == volumesDir {
		return fmt.Errorf("folder is already the title's volumes directory")
	}
	files := cbzFiles(srcDir)
	if len(files) == 0 {
		return fmt.Errorf("no .cbz files in %q", folder)
	}
	for _, f := range files {
		dst := filepath.Join(volumesDir, f)
		if _, err := os.Stat(dst); err == nil {
			continue // keep the existing volume; the leftover source file stays behind
		}
		if err := moveFile(filepath.Join(srcDir, f), dst); err != nil {
			return fmt.Errorf("move %s: %w", f, err)
		}
	}
	_ = os.Remove(srcDir) // only succeeds when emptied
	if _, err := s.repo.SyncVolumeFiles(ctx, title.ID, volumesDir, cbzPageCount); err != nil {
		return err
	}
	return s.GenerateVolumeThumbs(ctx, title.ID)
}

func moveFile(src, dst string) error {
	if err := renameFile(src, dst); err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

var renameFile = os.Rename
