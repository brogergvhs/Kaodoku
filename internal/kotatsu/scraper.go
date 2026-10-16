package kotatsu

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/providers"
)

const DefaultEndpoint = "http://localhost:8484"

const ScraperPrefix = "kotatsu:"

const urlSep = "\t"

type Scraper struct {
	c      *Client
	source string
}

func NewScraper(c *Client, source string) *Scraper {
	return &Scraper{c: c, source: source}
}

func (s *Scraper) GetChapters(ctx context.Context, url string) ([]providers.Chapter, error) {
	chs, _, err := s.GetChaptersResolved(ctx, url)
	return chs, err
}

func (s *Scraper) GetChaptersResolved(ctx context.Context, url string) ([]providers.Chapter, string, error) {
	res, err := s.c.Chapters(ctx, s.source, url)
	if err != nil {
		return nil, "", err
	}
	type kept struct {
		idx    int
		branch string
	}
	byLabel := map[string]kept{}
	out := make([]providers.Chapter, 0, len(res.Chapters))
	numbered := false
	for _, ch := range res.Chapters {
		if ch.Number != "" {
			numbered = true
			break
		}
	}
	perBranch := map[string]int{}
	for _, ch := range res.Chapters {
		number := ch.Number
		if number == "" {
			if numbered {
				continue
			}
			perBranch[ch.Branch]++
			number = strconv.Itoa(perBranch[ch.Branch])
		}
		main, suffixType, suffixNum, label, ok := providers.ParseChapterNumber(number)
		if !ok {
			continue
		}
		uid := ""
		if ch.ID != 0 {
			uid = strconv.FormatInt(ch.ID, 10)
		}
		next := providers.Chapter{
			URL:        res.ResolvedURL + urlSep + ch.URL,
			Title:      ch.Title,
			NumMain:    main,
			SuffixType: suffixType,
			SuffixNum:  suffixNum,
			Label:      label,
			Volume:     float64(ch.Volume),
			UID:        uid,
		}
		if k, seen := byLabel[label]; seen {
			if english(ch.Branch) && !english(k.branch) {
				out[k.idx] = next
				byLabel[label] = kept{k.idx, ch.Branch}
			}
			continue
		}
		byLabel[label] = kept{len(out), ch.Branch}
		out = append(out, next)
	}
	providers.SortChapters(out)
	return out, res.ResolvedURL, nil
}

func english(branch string) bool {
	return strings.HasPrefix(strings.ToLower(branch), "en")
}

func (s *Scraper) GetImages(ctx context.Context, chapterURL string) ([]string, error) {
	manga, chapter, ok := strings.Cut(chapterURL, urlSep)
	if !ok {
		return nil, fmt.Errorf("not a kotatsu chapter url: %q", chapterURL)
	}
	return s.c.Pages(ctx, s.source, manga, chapter)
}

func (s *Scraper) SearchManga(ctx context.Context, _ string, query string) ([]string, error) {
	res, err := s.c.Search(ctx, s.source, query)
	if err != nil {
		return nil, err
	}
	urls := make([]string, len(res))
	for i, r := range res {
		urls[i] = r.URL
	}
	return urls, nil
}
