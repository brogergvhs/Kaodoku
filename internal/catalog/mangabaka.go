package catalog

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/mangabaka"
)

const MangaBakaProvider = "mangabaka"

var mangaBakaStatus = map[string]string{"releasing": "RELEASING", "completed": "FINISHED", "cancelled": "CANCELLED", "hiatus": "HIATUS", "upcoming": "NOT_YET_RELEASED"}

// FromMangaBaka builds a record of a MangaBaka series under one provider's id, carrying every id it is known by.
func FromMangaBaka(sr mangabaka.Series, provider, id string) Manga {
	m := Manga{
		Provider: provider, ProviderID: id,
		TitleRomaji: cmp.Or(sr.Romanized, sr.Title), TitleEnglish: sr.English, TitleNative: sr.Native,
		Description: sr.Description, CoverImage: sr.CoverURL(350), Status: mangaBakaStatus[sr.Status], Format: "MANGA",
		Synonyms: sr.Titles, Genres: sr.Genres, Tags: sr.Tags, Authors: sr.Authors, Year: sr.Year,
		AverageScore: int(sr.Rating), IsAdult: sr.ContentRating == "pornographic",
		ExternalIDs: map[string]string{MangaBakaProvider: strconv.FormatInt(sr.ID, 10)},
	}
	if sr.Type == "novel" {
		m.Format = "NOVEL"
	}
	if sr.Chapters > 0 {
		m.Chapters = &sr.Chapters
	}
	if sr.Volumes > 0 {
		m.Volumes = &sr.Volumes
	}
	switch m.RelationType = strings.ToUpper(sr.Relation); m.RelationType {
	case "MAIN":
		m.RelationType = "PARENT"
	case "CHARACTER_FOCUS":
		m.RelationType = "CHARACTER"
	}
	maps.Copy(m.ExternalIDs, sr.IDs)
	return m
}

// MangaBakaClient serves metadata from the local MangaBaka copy, with no network calls.
type MangaBakaClient struct{ store *mangabaka.Store }

func NewMangaBakaClient(store *mangabaka.Store) *MangaBakaClient { return &MangaBakaClient{store} }

func (c *MangaBakaClient) Name() string  { return MangaBakaProvider }
func (c *MangaBakaClient) Label() string { return "MangaBaka" }

func (c *MangaBakaClient) manga(list []mangabaka.Series) []Manga {
	out := make([]Manga, len(list))
	for i, sr := range list {
		out[i] = FromMangaBaka(sr, MangaBakaProvider, strconv.FormatInt(sr.ID, 10))
	}
	return out
}

func (c *MangaBakaClient) Get(ctx context.Context, id string) (Manga, []Relation, error) {
	n, _ := strconv.ParseInt(id, 10, 64)
	sr, ok := c.store.ByIDs(ctx, []int64{n})[n]
	if !ok {
		return Manga{}, nil, ErrNotFound
	}
	rels, _ := c.store.Related(ctx, sr.ID, 50)
	out := make([]Relation, len(rels))
	for i, m := range c.manga(rels) {
		out[i] = Relation{ProviderID: m.ProviderID, Type: m.RelationType}
	}
	return c.manga([]mangabaka.Series{sr})[0], out, nil
}

func (c *MangaBakaClient) Search(ctx context.Context, query string, limit int, f SearchFilter) ([]Manga, bool, error) {
	sort := ""
	switch {
	case strings.Contains(f.Sort, "SCORE"):
		sort = "rating"
	case strings.Contains(f.Sort, "START_DATE"):
		sort = "newest"
	case strings.Contains(f.Sort, "POPULARITY"):
		sort = "popularity"
	}
	items, more := c.store.Search(ctx, query, mangabaka.Filter{GenreIn: f.GenreIn, GenreNotIn: f.GenreNotIn, TagIn: f.TagIn, TagNotIn: f.TagNotIn,
		Sort: sort, Limit: limit, Offset: (max(f.Page, 1) - 1) * limit})
	return c.manga(items), more, nil
}

func (c *MangaBakaClient) Trending(ctx context.Context, limit int) ([]Manga, error) {
	return c.manga(c.store.Trending(ctx, limit)), nil
}

func (c *MangaBakaClient) Related(ctx context.Context, id string, limit int) ([]Manga, []Manga, error) {
	n, _ := strconv.ParseInt(id, 10, 64)
	rels, recs := c.store.Related(ctx, n, limit)
	return c.manga(rels), c.manga(recs), nil
}

func (c *MangaBakaClient) Viewer(context.Context) (string, string, error) {
	return "", "", errors.ErrUnsupported
}
func (c *MangaBakaClient) ListEntries(context.Context) ([]Entry, error) {
	return nil, errors.ErrUnsupported
}
func (c *MangaBakaClient) GetEntry(context.Context, string) (Entry, bool, error) {
	return Entry{}, false, errors.ErrUnsupported
}
func (c *MangaBakaClient) SaveEntry(context.Context, string, EntryUpdate) error {
	return errors.ErrUnsupported
}
func (c *MangaBakaClient) DeleteEntry(context.Context, string) error { return errors.ErrUnsupported }
