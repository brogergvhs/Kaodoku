package kotatsu

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Live check against a running worker; set KAODOKU_KOTATSU_LIVE to
// "<endpoint>|<key>" to enable, e.g. "http://127.0.0.1:8484|change-me".
func TestClientLive(t *testing.T) {
	spec := os.Getenv("KAODOKU_KOTATSU_LIVE")
	if spec == "" {
		t.Skip("KAODOKU_KOTATSU_LIVE not set")
	}
	endpoint, key, _ := strings.Cut(spec, "|")
	c := New(endpoint, key)
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil || !h.OK || h.Parsers == 0 {
		t.Fatalf("Health = %+v, %v", h, err)
	}
	srcs, err := c.Sources(ctx)
	if err != nil || len(srcs) == 0 {
		t.Fatalf("Sources = %d, %v", len(srcs), err)
	}
	res, err := c.Search(ctx, "MANGADEX", "dai dark")
	if err != nil || len(res) == 0 {
		t.Fatalf("Search = %d, %v", len(res), err)
	}
	chs, err := c.Chapters(ctx, "MANGADEX", res[0].URL)
	if err != nil || len(chs.Chapters) == 0 || chs.ResolvedURL == "" {
		t.Fatalf("Chapters = %d resolved=%q, %v", len(chs.Chapters), chs.ResolvedURL, err)
	}
	pages, err := c.Pages(ctx, "MANGADEX", res[0].URL, chs.Chapters[0].URL)
	if err != nil || len(pages) == 0 {
		t.Fatalf("Pages = %d, %v", len(pages), err)
	}
	t.Logf("live: %d sources, %d chapters, %d pages, image %s", len(srcs), len(chs.Chapters), len(pages), pages[0])

	s := NewScraper(c, "MANGADEX")
	sch, resolved, err := s.GetChaptersResolved(ctx, res[0].URL)
	if err != nil || len(sch) == 0 || resolved == "" {
		t.Fatalf("scraper chapters = %d resolved=%q, %v", len(sch), resolved, err)
	}
	imgs, err := s.GetImages(ctx, sch[0].URL)
	if err != nil || len(imgs) == 0 {
		t.Fatalf("scraper images = %d, %v", len(imgs), err)
	}
	t.Logf("scraper live: %d chapters (first %q %q), %d images", len(sch), sch[0].Label, sch[0].Title, len(imgs))
}
