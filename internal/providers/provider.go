package providers

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type Chapter struct {
	URL        string
	Title      string
	NumMain    int
	SuffixType string
	SuffixNum  int
	Label      string
	Volume     float64
	UID        string
}

// SortChapters orders chapters by main number, then suffix type and number.
func SortChapters(chapters []Chapter) {
	sort.SliceStable(chapters, func(i, j int) bool {
		if chapters[i].NumMain != chapters[j].NumMain {
			return chapters[i].NumMain < chapters[j].NumMain
		}
		if chapters[i].SuffixType != chapters[j].SuffixType {
			return chapters[i].SuffixType < chapters[j].SuffixType
		}
		return chapters[i].SuffixNum < chapters[j].SuffixNum
	})
}

type Scraper interface {
	GetChapters(ctx context.Context, url string) ([]Chapter, error)
	GetImages(ctx context.Context, chapterURL string) ([]string, error)
}

// RedirectAware is an optional Scraper capability: chapter fetches also
// report the URL the page resolved to after redirects, so callers can persist
// rotated source URLs (AsuraScans rotates a hash suffix on series slugs).
type RedirectAware interface {
	GetChaptersResolved(ctx context.Context, url string) ([]Chapter, string, error)
}

// LanguageAware is an optional Scraper capability for multi-language sources:
// chapters restricted to preferred languages (optionally padded with any
// language), plus a count of chapters existing only outside them.
type LanguageAware interface {
	GetChaptersByLanguage(ctx context.Context, url string, preferred []string, includeAll bool) ([]Chapter, int, error)
}

// Searcher is an optional Scraper capability for sources with a native search
// API (rather than a scrapeable HTML results page). searchURL supplies the
// host; it returns manga page URLs.
type Searcher interface {
	SearchManga(ctx context.Context, searchURL, query string) ([]string, error)
}

// ParseChapterNumber splits a decimal chapter number ("12", "12.5") into the
// sort-key parts used across scrapers; the label keeps fraction text verbatim
// ("10.05" must not collapse to "10.5").
func ParseChapterNumber(raw string) (main int, suffixType string, suffixNum int, label string, ok bool) {
	raw = strings.TrimSpace(raw)
	separator := ""
	if strings.Contains(raw, ".") {
		separator = "."
	} else if strings.Contains(raw, "-") {
		separator = "-"
	}
	if separator == "" {
		main, err := strconv.Atoi(trimLeadingZeros(raw))
		return main, "", 0, strconv.Itoa(main), err == nil
	}
	parts := strings.SplitN(raw, separator, 2)
	main, errMain := strconv.Atoi(trimLeadingZeros(parts[0]))
	suffixText := strings.TrimSpace(parts[1])
	suffixNum, errSuffix := strconv.Atoi(trimLeadingZeros(suffixText))
	if errMain != nil || errSuffix != nil {
		return 0, "", 0, "", false
	}
	return main, separator, suffixNum, fmt.Sprintf("%d%s%s", main, separator, suffixText), true
}

func trimLeadingZeros(value string) string {
	value = strings.TrimLeft(strings.TrimSpace(value), "0")
	if value == "" {
		return "0"
	}
	return value
}

// ResolveURL resolves href against baseURL, tolerating malformed input.
func ResolveURL(baseURL, href string) string {
	if href == "" {
		return baseURL
	}

	u, err := url.Parse(href)
	if err == nil && u.IsAbs() {
		return u.String()
	}

	b, err := url.Parse(baseURL)
	if err != nil {
		return href
	}

	return b.ResolveReference(u).String()
}
