package browserdownload

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadCBZ(t *testing.T) {
	var got Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := readJSON(r, &got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("X-Kaodoku-Images", "2")
		_, _ = w.Write(testZip(t))
	}))
	defer server.Close()

	output := filepath.Join(t.TempDir(), "chapter.cbz")
	result, err := New(server.URL, time.Second, server.Client()).DownloadCBZ(context.Background(), Request{
		ChapterURL:        "https://manga.test/chapter-1",
		AllowedExtensions: []string{"webp"},
	}, output)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChapterURL != "https://manga.test/chapter-1" || got.AllowedExtensions[0] != "webp" {
		t.Fatalf("request = %#v", got)
	}
	if result.Images != 2 || result.Bytes == 0 {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadCBZSendsWorkerKey(t *testing.T) {
	t.Setenv("KAODOKU_BROWSER_DOWNLOADER_API_KEY", "sekrit")
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Worker-Key")
		_, _ = w.Write(testZip(t))
	}))
	defer server.Close()

	_, err := New(server.URL, time.Second, server.Client()).DownloadCBZ(context.Background(), Request{}, filepath.Join(t.TempDir(), "x.cbz"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "sekrit" {
		t.Fatalf("X-Worker-Key = %q", got)
	}
}

func TestDownloadCBZStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := New(server.URL, time.Second, server.Client()).DownloadCBZ(context.Background(), Request{}, filepath.Join(t.TempDir(), "x.cbz"))
	if err == nil {
		t.Fatal("DownloadCBZ() error = nil")
	}
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(dst)
}

func testZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("page_001.webp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("image")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
