package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadImagesReportsSampleErrors(t *testing.T) {
	d := New(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: http.NoBody}, nil
	})}, false)
	d.retryDelay = 0

	_, _, err := d.DownloadImagesConcurrently(
		context.Background(),
		[]string{"https://cdn.test/1.webp", "https://cdn.test/2.webp"},
		filepath.Join(t.TempDir(), "chapter"),
		"https://manga.test/chapter",
		1,
		noProgress{},
	)
	if err == nil {
		t.Fatal("DownloadImagesConcurrently() error = nil")
	}
	if !strings.Contains(err.Error(), "image 1: HTTP 403") {
		t.Fatalf("error = %v", err)
	}
}

func TestDownloadImagesAcceptsBinaryImageByURLExtension(t *testing.T) {
	d := New(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
			Body:       io.NopCloser(strings.NewReader("image bytes")),
		}, nil
	})}, false)
	d.retryDelay = 0

	files, _, err := d.DownloadImagesConcurrently(
		context.Background(),
		[]string{"https://i1.mangakatana.com/token/abc/0.jpg"},
		filepath.Join(t.TempDir(), "chapter"),
		"https://mangakatana.com/manga/title/c50",
		1,
		noProgress{},
	)
	if err != nil {
		t.Fatalf("DownloadImagesConcurrently() error = %v", err)
	}
	if len(files) != 1 || !strings.HasSuffix(files[0], "page_001.jpg") {
		t.Fatalf("files = %#v", files)
	}
}

func TestDownloadImagesWarmsBrowserSessionOnForbidden(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "cf_clearance=ok" {
			return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: http.NoBody}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"image/webp"}},
			Body:       io.NopCloser(strings.NewReader("image")),
		}, nil
	})}
	browser := &fakeBrowser{client: client, imageCookieTarget: "https://www.zazamanga.com/manga/the-demonic-supreme-sword/chapter-30"}
	d := New(client, false)
	d.SetBrowserFetcher(browser)
	d.retryDelay = 0

	files, _, err := d.DownloadImagesConcurrently(
		context.Background(),
		[]string{"https://img-r1.2xstorage.com/the-demonic-supreme-sword/30/0.webp"},
		filepath.Join(t.TempDir(), "chapter"),
		"https://www.zazamanga.com/manga/the-demonic-supreme-sword/chapter-30",
		1,
		noProgress{},
	)
	if err != nil {
		t.Fatalf("DownloadImagesConcurrently() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %#v", files)
	}
	if browser.calls != 1 {
		t.Fatalf("browser calls = %d", browser.calls)
	}
	if browser.targets[0] != "https://www.zazamanga.com/manga/the-demonic-supreme-sword/chapter-30" {
		t.Fatalf("browser targets = %#v", browser.targets)
	}
}

func TestDownloadImagesWarmsCDNOriginAfterRefererStillForbidden(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "cf_clearance=ok" {
			return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: http.NoBody}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"image/webp"}},
			Body:       io.NopCloser(strings.NewReader("image")),
		}, nil
	})}
	browser := &fakeBrowser{client: client, imageCookieTarget: "https://img-r1.2xstorage.com/"}
	d := New(client, false)
	d.SetBrowserFetcher(browser)
	d.retryDelay = 0

	_, _, err = d.DownloadImagesConcurrently(
		context.Background(),
		[]string{"https://img-r1.2xstorage.com/the-demonic-supreme-sword/30/0.webp"},
		filepath.Join(t.TempDir(), "chapter"),
		"https://www.zazamanga.com/manga/the-demonic-supreme-sword/chapter-30",
		1,
		noProgress{},
	)
	if err != nil {
		t.Fatalf("DownloadImagesConcurrently() error = %v", err)
	}
	want := "https://www.zazamanga.com/manga/the-demonic-supreme-sword/chapter-30,https://img-r1.2xstorage.com/"
	if strings.Join(browser.targets, ",") != want {
		t.Fatalf("browser targets = %#v", browser.targets)
	}
}

func TestDownloadImagesStopsAfterWarmFailure(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: http.NoBody}, nil
	})}
	browser := &fakeBrowser{client: client, failTarget: "https://img-r1.2xstorage.com/"}
	d := New(client, false)
	d.SetBrowserFetcher(browser)
	d.retryDelay = 0

	_, _, err = d.DownloadImagesConcurrently(
		context.Background(),
		[]string{
			"https://img-r1.2xstorage.com/the-demonic-supreme-sword/30/0.webp",
			"https://img-r1.2xstorage.com/the-demonic-supreme-sword/30/1.webp",
		},
		filepath.Join(t.TempDir(), "chapter"),
		"https://www.zazamanga.com/manga/the-demonic-supreme-sword/chapter-30",
		1,
		noProgress{},
	)
	if err == nil {
		t.Fatal("DownloadImagesConcurrently() error = nil")
	}
	if browser.calls != 2 {
		t.Fatalf("browser calls = %d", browser.calls)
	}
	if !strings.Contains(err.Error(), "browser warm failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestImageExtIgnoresURLQuery(t *testing.T) {
	got := imageExt("https://cdn.test/page.webp?v=1781373584")
	if got != ".webp" {
		t.Fatalf("imageExt() = %q", got)
	}
}

func TestSampleErrorsDeduplicatesImageFailures(t *testing.T) {
	got := sampleErrors([]error{
		fmt.Errorf("image 1: HTTP 403 (browser warm failed for cdn.test: blocked)"),
		fmt.Errorf("image 2: HTTP 403 (browser warm failed for cdn.test: blocked)"),
		fmt.Errorf("image 3: HTTP 403 (browser warm failed for cdn.test: blocked)"),
	}, 3)
	want := "image 1: HTTP 403 (browser warm failed for cdn.test: blocked); ..."
	if got != want {
		t.Fatalf("sampleErrors() = %q", got)
	}
}

type noProgress struct{}

func (noProgress) Update(int, int, int64) {}
func (noProgress) MarkDone()              {}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type fakeBrowser struct {
	client            *http.Client
	calls             int
	targets           []string
	imageCookieTarget string
	failTarget        string
}

func (f *fakeBrowser) LoadCached(context.Context, string) {}

func (f *fakeBrowser) Fetch(_ context.Context, target string) (string, error) {
	f.calls++
	f.targets = append(f.targets, target)
	u, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	if target == f.failTarget {
		return "", fmt.Errorf("solver failed")
	}
	f.client.Jar.SetCookies(u, []*http.Cookie{{Name: "cf_clearance", Value: "ok", Path: "/"}})
	if target == f.imageCookieTarget {
		imageHost, err := url.Parse("https://img-r1.2xstorage.com/")
		if err != nil {
			return "", err
		}
		f.client.Jar.SetCookies(imageHost, []*http.Cookie{{Name: "cf_clearance", Value: "ok", Path: "/"}})
	}
	return "", nil
}

func TestSniffImageExt(t *testing.T) {
	t.Parallel()

	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	jpg := append([]byte("\xff\xd8\xff\xe0"), make([]byte, 16)...)

	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"webp", webp, ".webp"},
		{"png", png, ".png"},
		{"jpeg stays .jpg", jpg, ".jpg"},
		{"unknown", []byte("not an image at all"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "page")
			if err := os.WriteFile(file, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := sniffImageExt(file); got != tc.want {
				t.Errorf("sniffImageExt() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := sniffImageExt(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Errorf("missing file = %q, want \"\"", got)
	}
}
