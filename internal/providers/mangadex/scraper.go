// Package mangadex reads MangaDex's public JSON API. The site itself is a JS
// SPA, so there is deliberately no HTML fallback. Image URLs come from the
// at-home endpoint whose CDN node assignment is short-lived, so they are
// resolved per chapter at download time.
package mangadex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/providers/generic"
	"github.com/brogergvhs/kaodoku/internal/ui"
	"github.com/brogergvhs/kaodoku/internal/util"
)

const (
	apiBase    = "https://api.mangadex.org"
	feedPage   = 500
	maxFeeds   = 40 // 20k chapters
	allRatings = "&contentRating[]=safe&contentRating[]=suggestive&contentRating[]=erotica&contentRating[]=pornographic"
)

type Scraper struct {
	client *http.Client
	log    ui.Log
}

func NewScraper(c *http.Client, log ui.Log, _ []string, _ bool, _ generic.BrowserFetcher) *Scraper {
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &Scraper{client: c, log: log}
}

func (s *Scraper) GetChapters(ctx context.Context, pageURL string) ([]providers.Chapter, error) {
	chapters, _, err := s.GetChaptersByLanguage(ctx, pageURL, []string{"en"}, false)
	return chapters, err
}

// GetChaptersByLanguage returns chapters in the preferred languages (best
// preference wins per chapter). gap counts chapters existing only in other
// languages; includeAll pulls those in too, via the aggregate endpoint.
func (s *Scraper) GetChaptersByLanguage(ctx context.Context, pageURL string, preferred []string, includeAll bool) ([]providers.Chapter, int, error) {
	id, err := pathUUID(pageURL, "title")
	if err != nil {
		return nil, 0, err
	}
	if len(preferred) == 0 {
		preferred = []string{"en"}
	}
	rank := make(map[string]int, len(preferred))
	langFilter := ""
	for i, lang := range preferred {
		rank[lang] = i
		langFilter += "&translatedLanguage[]=" + url.QueryEscape(lang)
	}

	type pick struct {
		chapter providers.Chapter
		rank    int
	}
	best := map[string]pick{}
	skipped := 0
	for offset := 0; offset < maxFeeds*feedPage; offset += feedPage {
		var resp struct {
			Data []struct {
				ID         string `json:"id"`
				Attributes struct {
					Chapter            string `json:"chapter"`
					Volume             string `json:"volume"`
					Title              string `json:"title"`
					Pages              int    `json:"pages"`
					ExternalURL        string `json:"externalUrl"`
					TranslatedLanguage string `json:"translatedLanguage"`
				} `json:"attributes"`
			} `json:"data"`
			Total int `json:"total"`
		}
		feed := fmt.Sprintf("%s/manga/%s/feed?order[chapter]=asc&limit=%d&offset=%d%s%s", apiBase, id, feedPage, offset, langFilter, allRatings)
		if err := util.GetJSON(ctx, s.client, feed, &resp); err != nil {
			return nil, 0, err
		}
		for _, c := range resp.Data {
			a := c.Attributes
			if a.ExternalURL != "" || a.Pages == 0 {
				skipped++
				continue
			}
			main, suffixType, suffixNum, label := 0, "", 0, "Oneshot"
			if a.Chapter != "" {
				var ok bool
				main, suffixType, suffixNum, label, ok = providers.ParseChapterNumber(a.Chapter)
				if !ok {
					continue
				}
			}
			langRank, ok := rank[a.TranslatedLanguage]
			if !ok {
				langRank = len(preferred)
			}
			if cur, exists := best[label]; exists && cur.rank <= langRank {
				continue
			}
			title := "Chapter " + label
			if t := strings.TrimSpace(a.Title); t != "" {
				title += " - " + t
			}
			vol, _ := strconv.ParseFloat(a.Volume, 64)
			best[label] = pick{rank: langRank, chapter: providers.Chapter{
				URL:        "https://mangadex.org/chapter/" + c.ID,
				Title:      title,
				NumMain:    main,
				SuffixType: suffixType,
				SuffixNum:  suffixNum,
				Label:      label,
				Volume:     vol,
				UID:        c.ID,
			}}
		}
		if offset+feedPage >= resp.Total {
			break
		}
	}
	if skipped > 0 && s.log != nil {
		s.log.Infof("Skipped %d external/pageless MangaDex chapters.\n", skipped)
	}

	gap := 0
	all, err := s.aggregateChapters(ctx, id)
	if err != nil && s.log != nil {
		s.log.Debugf("MangaDex aggregate failed for %s: %v\n", id, err)
	}
	for label, agg := range all {
		if _, ok := best[label]; ok {
			continue
		}
		gap++
		if !includeAll {
			continue
		}
		main, suffixType, suffixNum, parsedLabel := 0, "", 0, "Oneshot"
		if label != "Oneshot" {
			var ok bool
			main, suffixType, suffixNum, parsedLabel, ok = providers.ParseChapterNumber(label)
			if !ok {
				continue
			}
		}
		best[label] = pick{chapter: providers.Chapter{
			URL:        "https://mangadex.org/chapter/" + agg.id,
			Title:      "Chapter " + parsedLabel,
			NumMain:    main,
			SuffixType: suffixType,
			SuffixNum:  suffixNum,
			Label:      parsedLabel,
			Volume:     agg.volume,
			UID:        agg.id,
		}}
	}
	if includeAll {
		gap = 0
	}

	if len(best) == 0 && (skipped > 0 || gap > 0) {
		return nil, gap, fmt.Errorf("MangaDex lists this manga but has no downloadable chapters in the preferred languages (%s) — entries are external/officially licensed or exist only in other languages", strings.Join(preferred, ", "))
	}
	out := make([]providers.Chapter, 0, len(best))
	for _, p := range best {
		out = append(out, p.chapter)
	}
	providers.SortChapters(out)
	return out, gap, nil
}

// aggregateChapters maps every chapter label (any language) to one chapter id.
type aggregateChapter struct {
	id     string
	volume float64
}

func (s *Scraper) aggregateChapters(ctx context.Context, id string) (map[string]aggregateChapter, error) {
	var resp struct {
		Volumes map[string]struct {
			Chapters map[string]struct {
				ID string `json:"id"`
			} `json:"chapters"`
		} `json:"volumes"`
	}
	if err := util.GetJSON(ctx, s.client, apiBase+"/manga/"+id+"/aggregate", &resp); err != nil {
		return nil, err
	}
	out := map[string]aggregateChapter{}
	for volKey, v := range resp.Volumes {
		vol, _ := strconv.ParseFloat(volKey, 64)
		for num, c := range v.Chapters {
			label := "Oneshot"
			if num != "" && num != "none" {
				if _, _, _, parsed, ok := providers.ParseChapterNumber(num); ok {
					label = parsed
				} else {
					continue
				}
			}
			if _, dup := out[label]; !dup {
				out[label] = aggregateChapter{id: c.ID, volume: vol}
			}
		}
	}
	return out, nil
}

func (s *Scraper) GetImages(ctx context.Context, chapterURL string) ([]string, error) {
	id, err := pathUUID(chapterURL, "chapter")
	if err != nil {
		return nil, err
	}
	var resp struct {
		BaseURL string `json:"baseUrl"`
		Chapter struct {
			Hash string   `json:"hash"`
			Data []string `json:"data"`
		} `json:"chapter"`
	}
	if err := util.GetJSON(ctx, s.client, apiBase+"/at-home/server/"+id, &resp); err != nil {
		return nil, err
	}
	if resp.BaseURL == "" || len(resp.Chapter.Data) == 0 {
		return nil, fmt.Errorf("no pages for mangadex chapter %s", id)
	}
	out := make([]string, 0, len(resp.Chapter.Data))
	for _, file := range resp.Chapter.Data {
		out = append(out, resp.BaseURL+"/data/"+resp.Chapter.Hash+"/"+file)
	}
	return out, nil
}

// SearchManga queries the manga endpoint and returns site title-page URLs.
func (s *Scraper) SearchManga(ctx context.Context, searchURL, query string) ([]string, error) {
	target := strings.ReplaceAll(searchURL, "{query}", url.QueryEscape(query))
	if !strings.Contains(searchURL, "{query}") {
		target = apiBase + "/manga?limit=10&title=" + url.QueryEscape(query)
	}
	target += allRatings
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := util.GetJSON(ctx, s.client, target, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, "https://mangadex.org/title/"+m.ID)
	}
	return out, nil
}

// pathUUID extracts the UUID following a path marker like /title/ or /chapter/.
func pathUUID(rawURL, marker string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == marker && i+1 < len(parts) && len(parts[i+1]) == 36 {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("no %s id in %s", marker, rawURL)
}
