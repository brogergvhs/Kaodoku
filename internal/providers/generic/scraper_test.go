package generic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/ui"
)

func TestFetchBodyCloudflareUsesBrowserSolver(t *testing.T) {
	scraper := NewScraper(cfClient(), ui.NewLogger(false), nil, false, fakeBrowserFetcher("<html>solved</html>"))
	body, _, fromBrowser, err := scraper.fetchBody(context.Background(), "http://manga.test")
	if err != nil {
		t.Fatal(err)
	}
	if body != "<html>solved</html>" || !fromBrowser {
		t.Fatalf("body = %q fromBrowser = %v", body, fromBrowser)
	}
}

func TestFetchBodyCloudflareWithoutBrowserSolver(t *testing.T) {
	scraper := NewScraper(cfClient(), ui.NewLogger(false), nil, false, nil)
	if _, _, _, err := scraper.fetchBody(context.Background(), "http://manga.test"); err == nil {
		t.Fatal("fetchBody() error = nil")
	} else if !strings.Contains(err.Error(), "browser_solver.enabled") {
		t.Fatalf("fetchBody() error = %v", err)
	}
}

func TestFetchBodyCloudflareBlockBodyUsesBrowserSolver(t *testing.T) {
	scraper := NewScraper(statusClient(http.StatusOK, "Attention Required! | Cloudflare\nSorry, you have been blocked"), ui.NewLogger(false), nil, false, fakeBrowserFetcher("<html>solved</html>"))
	body, _, fromBrowser, err := scraper.fetchBody(context.Background(), "http://manga.test")
	if err != nil {
		t.Fatal(err)
	}
	if body != "<html>solved</html>" || !fromBrowser {
		t.Fatalf("body = %q fromBrowser = %v", body, fromBrowser)
	}
}

func TestFetchBodyHTTPErrorUsesBrowserSolver(t *testing.T) {
	scraper := NewScraper(statusClient(http.StatusInternalServerError, "500 Internal Server Error"), ui.NewLogger(false), nil, false, fakeBrowserFetcher("<html>solved</html>"))
	body, _, fromBrowser, err := scraper.fetchBody(context.Background(), "http://manga.test")
	if err != nil {
		t.Fatal(err)
	}
	if body != "<html>solved</html>" || !fromBrowser {
		t.Fatalf("body = %q fromBrowser = %v", body, fromBrowser)
	}
}

func TestFetchBodyHTTPErrorWithoutBrowserSolver(t *testing.T) {
	scraper := NewScraper(statusClient(http.StatusInternalServerError, "500 Internal Server Error"), ui.NewLogger(false), nil, false, nil)
	if _, _, _, err := scraper.fetchBody(context.Background(), "http://manga.test"); err == nil {
		t.Fatal("fetchBody() error = nil")
	} else if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("fetchBody() error = %v", err)
	}
}

func TestGetImagesFetchesChapterPageOnce(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(bytes.NewBufferString(`
				<html><body><img src="https://cdn.test/page-001.webp"></body></html>
			`)),
			Header: http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), []string{"webp"}, false, nil)
	images, err := scraper.GetImages(context.Background(), "http://manga.test/chapter-1")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls)
	}
	if len(images) != 1 || images[0] != "https://cdn.test/page-001.webp" {
		t.Fatalf("images = %#v", images)
	}
}

func TestGetImagesScansMultiPageChapter(t *testing.T) {
	var calls []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Path)
		body := map[string]string{
			"/manga/title/c270/": `
				<html><body>
					<select>
						<option value="/manga/title/c270/">1</option>
						<option value="/manga/title/c270/2.html">2</option>
						<option value="/manga/title/c270/3.html">3</option>
					</select>
					<img src="https://cdn.test/page-001.jpg">
				</body></html>
			`,
			"/manga/title/c270/2.html": `<html><body><img src="https://cdn.test/page-002.jpg"></body></html>`,
			"/manga/title/c270/3.html": `<html><body><img src="https://cdn.test/page-003.jpg"></body></html>`,
		}[req.URL.Path]
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), []string{"jpg"}, false, nil)
	images, err := scraper.GetImages(context.Background(), "http://manga.test/manga/title/c270/")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "/manga/title/c270/,/manga/title/c270/2.html,/manga/title/c270/3.html" {
		t.Fatalf("calls = %#v", calls)
	}
	want := []string{
		"https://cdn.test/page-001.jpg",
		"https://cdn.test/page-002.jpg",
		"https://cdn.test/page-003.jpg",
	}
	if strings.Join(images, ",") != strings.Join(want, ",") {
		t.Fatalf("images = %#v", images)
	}
}

func TestGetImagesUsesBrowserRenderedHTMLForDynamicApp(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(bytes.NewBufferString(`
				<html><body>
					<div id="app-root"></div>
					<script id="initial-data" type="application/json">{"poster":"https://static.test/poster.jpg"}</script>
				</body></html>
			`)),
			Header: http.Header{},
		}, nil
	})}
	browser := fakeBrowserFetcher(`
		<html><body>
			<img src="https://cdn.test/page-001.webp">
			<img src="https://cdn.test/page-002.webp">
		</body></html>
	`)

	scraper := NewScraper(client, ui.NewLogger(false), []string{"webp", "jpg"}, false, browser)
	images, err := scraper.GetImages(context.Background(), "https://comix.to/title/vyd0/7266081-chapter-30")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://cdn.test/page-001.webp", "https://cdn.test/page-002.webp"}
	if strings.Join(images, ",") != strings.Join(want, ",") {
		t.Fatalf("images = %#v", images)
	}
}

func TestGetImagesUsesBrowserRenderedHTMLWhenStaticHasOnlyChromeAssets(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(bytes.NewBufferString(`
				<html><body>
					<img src="https://comickz.co.uk/images/ads/ori-expand.png">
					<img src="https://cdn1.comicknew.pictures/title/covers/cover.webp">
				</body></html>
			`)),
			Header: http.Header{},
		}, nil
	})}
	browser := fakeBrowserFetcher(`
		<html><body>
			<img src="https://cdn1.comicknew.pictures/title/chapter-30/001.webp" data-kaodoku-page-image="1">
			<img src="https://cdn1.comicknew.pictures/title/chapter-30/002.webp" data-kaodoku-page-image="1">
		</body></html>
	`)

	scraper := NewScraper(client, ui.NewLogger(false), []string{"webp", "png"}, false, browser)
	images, err := scraper.GetImages(context.Background(), "https://comickz.co.uk/comic/title/hash-chapter-30-en")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://cdn1.comicknew.pictures/title/chapter-30/001.webp",
		"https://cdn1.comicknew.pictures/title/chapter-30/002.webp",
	}
	if strings.Join(images, ",") != strings.Join(want, ",") {
		t.Fatalf("images = %#v", images)
	}
}

func TestGetImagesFetchesHTMXImageFragment(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `
			<html><body>
				<img src="/static/images/brand.png">
				<meta property="og:image" content="https://temp.test/cover/fallback/title.jpg">
				<section hx-get="/chapters/abc/images?is_prev=False&amp;current_page=1" hx-include="[name='reading_style']"></section>
			</body></html>
		`
		if req.URL.Path == "/chapters/abc/images" {
			if req.Header.Get("HX-Request") != "true" {
				t.Fatalf("HX-Request header = %q", req.Header.Get("HX-Request"))
			}
			if req.URL.Query().Get("reading_style") != "long_strip" {
				t.Fatalf("reading_style = %q", req.URL.Query().Get("reading_style"))
			}
			body = `
				<img src="https://cdn.test/chapter/001.png">
				<img src="https://cdn.test/chapter/002.png">
			`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), []string{"png", "jpg"}, false, nil)
	images, err := scraper.GetImages(context.Background(), "https://weebcentral.com/chapters/abc")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://cdn.test/chapter/001.png", "https://cdn.test/chapter/002.png"}
	if strings.Join(images, ",") != strings.Join(want, ",") {
		t.Fatalf("images = %#v", images)
	}
}

func TestGetChaptersSkipsOtherSeriesLinks(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(bytes.NewBufferString(`
				<html><body>
					<a href="/manga/the-demonic-supreme-sword/chapter-1">Chapter 1</a>
					<a href="/manga/the-demonic-supreme-sword/chapter-18-1">Chapter 18.1</a>
					<a href="/manga/solo-leveling/chapter-200">Chapter 200</a>
					<a href="/manga/one-piece/chapter-1186">Chapter 1186</a>
				</body></html>
			`)),
			Header: http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	chapters, err := scraper.GetChapters(context.Background(), "https://www.zazamanga.com/manga/the-demonic-supreme-sword")
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 {
		t.Fatalf("chapters = %#v", chapters)
	}
	if chapters[0].Label != "1" || chapters[1].Label != "18-1" {
		t.Fatalf("chapters = %#v", chapters)
	}
}

func TestGetChaptersUsesBrowserRenderedHTML(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(bytes.NewBufferString(`
				<html><body><div id="app-root"></div><script id="initial-data" type="application/json">{}</script></body></html>
			`)),
			Header: http.Header{},
		}, nil
	})}
	browser := fakeBrowserFetcher(`
		<section class="mpage__chapters">
			<a href="/title/vyd0-the-returned-c-rank-tank-wont-die/10293037-chapter-60">Ch.60</a>
			<a href="/title/vyd0-the-returned-c-rank-tank-wont-die/10234746-chapter-59">Ch.59</a>
			<a href="/title/vyd0-the-returned-c-rank-tank-wont-die/10275554-chapter-59">Ch.59</a>
		</section>
	`)

	scraper := NewScraper(client, ui.NewLogger(false), nil, false, browser)
	chapters, err := scraper.GetChapters(context.Background(), "https://comix.to/title/vyd0-the-returned-c-rank-tank-wont-die")
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 {
		t.Fatalf("chapters = %#v", chapters)
	}
	if chapters[0].Label != "59" || chapters[1].Label != "60" {
		t.Fatalf("chapters = %#v", chapters)
	}
}

func TestGetChaptersAllowsSharedSeriesIDChapterPath(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(bytes.NewBufferString(`
				<html><body>
					<a href="/chapters/5281-10265000/sakamoto-days-chapter-265">Chapter 265</a>
					<a href="/chapters/9999-10264000/other-title-chapter-264">Chapter 264</a>
				</body></html>
			`)),
			Header: http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	chapters, err := scraper.GetChapters(context.Background(), "https://mangapill.com/manga/5281/sakamoto-days")
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 || chapters[0].Label != "265" {
		t.Fatalf("chapters = %#v", chapters)
	}
}

func TestGetChaptersAllowsEpisodeTextWithCentralChapterPath(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(bytes.NewBufferString(`
				<html><body>
					<a href="/chapters/01KVQMY1RKZSC02446EPA774WA">
						<span>Episode 262</span>
					</a>
					<a href="/chapters/not-a-chapter">Discussion</a>
				</body></html>
			`)),
			Header: http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	chapters, err := scraper.GetChapters(context.Background(), "https://weebcentral.com/series/01J76XYDMTRNJZJH9G1ADMPJQC/The-Great-Mage-Returns-After-4000-Years")
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 {
		t.Fatalf("chapters = %#v", chapters)
	}
	if chapters[0].Label != "262" || chapters[0].Title != "Episode 262" {
		t.Fatalf("chapters = %#v", chapters)
	}
}

func TestGetChaptersExpandsGappedChapterList(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `
			<html><body>
				<a href="/chapters/one"><span>Episode 1</span></a>
				<a href="/chapters/two"><span>Episode 2</span></a>
				<a href="/chapters/three"><span>Episode 3</span></a>
				<a href="/chapters/last"><span>Episode 20</span></a>
				<button hx-get="/series/title/full-chapter-list" hx-target="#chapter-list">Show All Chapters</button>
			</body></html>
		`
		if req.URL.Path == "/series/title/full-chapter-list" {
			body = `
				<a href="/chapters/one"><span>Episode 1</span></a>
				<a href="/chapters/two"><span>Episode 2</span></a>
				<a href="/chapters/three"><span>Episode 3</span></a>
				<a href="/chapters/four"><span>Episode 4</span></a>
				<a href="/chapters/last"><span>Episode 20</span></a>
			`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	chapters, err := scraper.GetChapters(context.Background(), "https://weebcentral.com/series/title")
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 5 || chapters[3].Label != "4" {
		t.Fatalf("chapters = %#v", chapters)
	}
}

func TestGetChaptersDynamicAppNeedsBrowser(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewBufferString(`<html><body><div id="app-root"></div></body></html>`)),
			Header:     http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	if _, err := scraper.GetChapters(context.Background(), "https://comix.to/title/vyd0-title"); err == nil {
		t.Fatal("GetChapters() error = nil")
	} else if !strings.Contains(err.Error(), "browser_solver.enabled") {
		t.Fatalf("GetChapters() error = %v", err)
	}
}

type fakeBrowserFetcher string

func (f fakeBrowserFetcher) Fetch(context.Context, string) (string, error) {
	return string(f), nil
}

func statusClient(code int, status string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Status:     status,
			Body:       io.NopCloser(bytes.NewBufferString(status)),
			Header:     http.Header{},
		}, nil
	})}
}

func cfClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Body:       io.NopCloser(bytes.NewBufferString("Just a moment")),
			Header:     http.Header{},
		}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// MangaThemesia sites list chapters under a different path root whose slug
// embeds the series slug: /series/{slug} -> /chapter/{slug}-chapter-N.
func TestSameSeriesChapterLinkAcceptsEmbeddedSlug(t *testing.T) {
	page := "https://rizzfables.com/series/r2311170-top-tier-providence"
	cases := []struct {
		href string
		want bool
	}{
		{"https://rizzfables.com/chapter/r2311170-top-tier-providence-chapter-225", true},
		{"https://rizzfables.com/chapter/r2311170-top-tier-providence-chapter-223.5", true},
		{"https://rizzfables.com/chapter/other-series-chapter-1", false},
		{"https://rizzfables.com/read/r2311170-top-tier-providence/en/chapter-3", true},
		{"https://other-site.com/chapter/r2311170-top-tier-providence-chapter-225", false},
	}
	for _, tc := range cases {
		if got := sameSeriesChapterLink(page, tc.href); got != tc.want {
			t.Errorf("sameSeriesChapterLink(%q) = %v, want %v", tc.href, got, tc.want)
		}
	}
}

// MangaFire embeds the series slug as an exact path segment of chapter URLs:
// /title/{slug} -> /read/{slug}/en/chapter-N.
func TestSameSeriesChapterLinkAcceptsSlugSegment(t *testing.T) {
	page := "https://mangafire.to/title/k39vr-edens-last-stand"
	if !sameSeriesChapterLink(page, "https://mangafire.to/read/k39vr-edens-last-stand/en/chapter-12") {
		t.Fatal("slug-segment chapter link rejected")
	}
	if sameSeriesChapterLink(page, "https://mangafire.to/read/other-manga/en/chapter-12") {
		t.Fatal("foreign chapter link accepted")
	}
}

// WeebCentral shows the first few chapters plus the latest few, so the hidden
// middle can be small; an explicit "Show All Chapters" control must be
// followed regardless of how the visible numbering looks.
func TestGetChaptersExpandsSmallGapBehindShowAll(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `
			<html><body>
				<a href="/chapters/a"><span>Episode 1</span></a>
				<a href="/chapters/b"><span>Episode 2</span></a>
				<a href="/chapters/c"><span>Episode 3</span></a>
				<a href="/chapters/x"><span>Episode 14</span></a>
				<a href="/chapters/y"><span>Episode 15</span></a>
				<button hx-get="/series/title/full-chapter-list" hx-target="#chapter-list">Show All Chapters</button>
			</body></html>
		`
		if req.URL.Path == "/series/title/full-chapter-list" {
			var sb strings.Builder
			for i := 1; i <= 15; i++ {
				fmt.Fprintf(&sb, `<a href="/chapters/n%d"><span>Episode %d</span></a>`, i, i)
			}
			body = sb.String()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     http.Header{},
		}, nil
	})}

	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	chapters, err := scraper.GetChapters(context.Background(), "https://weebcentral.com/series/title")
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 15 {
		t.Fatalf("chapters = %d, want 15", len(chapters))
	}
}

// Some MangaThemesia sites hang chapters off the site root with the series
// slug prefixed: /manga/{slug}/ -> /{slug}-84/.
func TestSameSeriesChapterLinkAcceptsRootLevelSlugPrefix(t *testing.T) {
	page := "https://arenascan.com/manga/wail-of-weakness/"
	if !sameSeriesChapterLink(page, "https://arenascan.com/wail-of-weakness-84/") {
		t.Fatal("root-level slug-prefixed chapter link rejected")
	}
	if sameSeriesChapterLink(page, "https://arenascan.com/other-series-84/") {
		t.Fatal("foreign root-level chapter link accepted")
	}
}
