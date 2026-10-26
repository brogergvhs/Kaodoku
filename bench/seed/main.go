package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	chaptersPkg "github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/providers"
)

func main() {
	dir := flag.String("dir", "bench/data", "database directory, wiped first")
	cbz := flag.String("cbz", "", "CBZ directory, never wiped (default <dir>/cbz)")
	titles := flag.Int("titles", 10, "titles")
	chapters := flag.Int("chapters", 10, "chapters per title")
	pages := flag.Int("pages", 20, "pages per chapter")
	pageKB := flag.Int("page-kb", 150, "page image size in KB")
	flag.Parse()
	if *cbz == "" {
		*cbz = filepath.Join(*dir, "cbz")
	}
	if err := seed(context.Background(), *dir, *cbz, *titles, *chapters, *pages, *pageKB<<10); err != nil {
		log.Fatal(err)
	}
}

func seed(ctx context.Context, dir, cbz string, titles, chapters, pages, pageBytes int) error {
	cbz, err := filepath.Abs(cbz)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	db, err := database.Open(ctx, filepath.Join(dir, "kaodoku.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		return err
	}
	for _, key := range []string{"serve.refresh_every", "serve.scan_every", "serve.download_every", "sources.verify_every", "catalog.refresh_every"} {
		if _, err := db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, '0')", key); err != nil {
			return err
		}
	}
	repo := library.NewRepository(db)
	img := make([]byte, pageBytes)
	_, _ = rand.Read(img)
	var total int64
	for t := 1; t <= titles; t++ {
		folder := "title-" + strconv.Itoa(t)
		title, err := repo.AddTitle(ctx, library.AddTitleParams{SourceURL: "local:" + folder, DisplayTitle: fmt.Sprintf("Bench Title %d", t), OutputPath: folder})
		if err != nil {
			return err
		}
		list := make([]chaptersPkg.Chapter, chapters)
		for c := range list {
			label := strconv.Itoa(c + 1)
			list[c] = chaptersPkg.Chapter{Chapter: providers.Chapter{URL: "local:" + folder + "/" + label + ".cbz", Label: label, NumMain: c + 1}}
		}
		if _, err := repo.UpsertChapters(ctx, title.ID, list); err != nil {
			return err
		}
		for c := 1; c <= chapters; c++ {
			label := strconv.Itoa(c)
			ch, err := repo.GetChapterByLabel(ctx, title.ID, label)
			if err != nil {
				return err
			}
			path := filepath.Join(cbz, folder, label+".cbz")
			size, err := writeCBZ(path, pages, img)
			if err != nil {
				return err
			}
			if err := repo.MarkDownloadCompleted(ctx, ch.ID, path, size, pages); err != nil {
				return err
			}
			total += size
		}
	}
	fmt.Printf("seeded %s: %d titles x %d chapters x %d pages, %d MB of CBZ in %s\n", dir, titles, chapters, pages, total>>20, cbz)
	return nil
}

func writeCBZ(path string, pages int, img []byte) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for p := 1; p <= pages; p++ {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%03d.jpg", p), Method: zip.Store})
		if err != nil {
			return 0, err
		}
		if _, err := w.Write(img); err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
