package util

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComicInfoRoundTrip(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "001.jpg")
	if err := os.WriteFile(img, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := MarshalComicInfo(ComicInfo{
		Series: "Berserk & Co", Title: "Lost <Children>", Number: "7.5",
		Summary: "dark", Writer: "Miura", Manga: "Yes", PageCount: 1, Volume: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Berserk &amp; Co") || !strings.Contains(string(body), "Lost &lt;Children&gt;") {
		t.Fatalf("escaping broken: %s", body)
	}
	if !strings.Contains(string(body), "<Volume>3</Volume>") {
		t.Fatalf("volume missing: %s", body)
	}

	cbz := filepath.Join(dir, "ch.cbz")
	if err := CreateCBZ([]string{img}, cbz, false, map[string][]byte{ComicInfoName: body}); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(cbz)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if entries := CBZImageEntries(zr.File); len(entries) != 1 || entries[0].Name != "001.jpg" {
		t.Fatalf("ComicInfo.xml leaked into image entries: %v", entries)
	}

	ci, ok := ReadComicInfo(cbz)
	if !ok || ci.Series != "Berserk & Co" || ci.Number != "7.5" || ci.Title != "Lost <Children>" {
		t.Fatalf("ReadComicInfo = %+v, %v", ci, ok)
	}
	if _, ok := ReadComicInfo(img); ok {
		t.Fatal("non-zip must not parse")
	}
}
