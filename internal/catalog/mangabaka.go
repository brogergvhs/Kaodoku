package catalog

import (
	"cmp"
	"context"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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

// MangaBakaClient serves metadata from the local MangaBaka copy with no network calls, and tracks reading through
// MangaBaka's API with a personal access token.
type MangaBakaClient struct {
	restClient
	store *mangabaka.Store
}

func NewMangaBakaClient(store *mangabaka.Store, client *http.Client) *MangaBakaClient {
	c := &MangaBakaClient{restClient: newRest("https://api.mangabaka.org/v1", "mangabaka", client, 350*time.Millisecond), store: store}
	c.auth = func(ctx context.Context, h http.Header) { h.Set("x-api-key", AccountFrom(ctx).Token) }
	return c
}

// Outage stays empty: metadata is local, and API errors show on the account instead.
func (c *MangaBakaClient) Outage() string { return "" }

// TokenLogin marks trackers that sign in with a pasted access token.
type TokenLogin interface{ TokenLogin() }

func (c *MangaBakaClient) TokenLogin()       {}
func (c *MangaBakaClient) TracksDates() bool { return true }

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

func (c *MangaBakaClient) Viewer(ctx context.Context) (string, string, error) {
	ctx = asCall(ctx, "Account")
	var resp struct {
		Data struct {
			ID                string `json:"id"`
			Nickname          string `json:"nickname"`
			PreferredUsername string `json:"preferred_username"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/my/profile", nil, &resp); err != nil {
		return "", "", err
	}
	return resp.Data.ID, cmp.Or(resp.Data.PreferredUsername, resp.Data.Nickname, resp.Data.ID), nil
}

var mangaBakaStates = map[string]string{"reading": "CURRENT", "completed": "COMPLETED", "plan_to_read": "PLANNING", "considering": "PLANNING",
	"dropped": "DROPPED", "paused": "PAUSED", "rereading": "REPEATING"}

type mangaBakaEntry struct {
	SeriesID int64 `json:"series_id"`
	Series   struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"series"`
	State      string   `json:"state"`
	Rating     *float64 `json:"rating"`
	Chapter    *float64 `json:"progress_chapter"`
	StartDate  string   `json:"start_date"`
	FinishDate string   `json:"finish_date"`
}

func (e mangaBakaEntry) entry() Entry {
	out := Entry{Status: mangaBakaStates[e.State]}
	if e.Chapter != nil {
		out.Progress = int(*e.Chapter)
	}
	if e.Rating != nil {
		out.Score = roundScore(*e.Rating / 10)
	}
	out.Started, _ = time.Parse(time.DateOnly, e.StartDate)
	out.Finished, _ = time.Parse(time.DateOnly, e.FinishDate)
	id := cmp.Or(e.SeriesID, e.Series.ID)
	out.Manga = Manga{Provider: MangaBakaProvider, ProviderID: strconv.FormatInt(id, 10), TitleRomaji: e.Series.Title}
	return out
}

func (c *MangaBakaClient) ListEntries(ctx context.Context) ([]Entry, error) {
	ctx = asCall(ctx, "List")
	var out []Entry
	for page := 1; page <= 100; page++ {
		var resp struct {
			Data       []mangaBakaEntry `json:"data"`
			Pagination struct {
				Next string `json:"next"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, "/my/library?limit=100&page="+strconv.Itoa(page), nil, &resp); err != nil {
			return nil, err
		}
		for _, e := range resp.Data {
			out = append(out, e.entry())
		}
		if resp.Pagination.Next == "" {
			break
		}
	}
	return out, nil
}

func (c *MangaBakaClient) GetEntry(ctx context.Context, id string) (Entry, bool, error) {
	ctx = asCall(ctx, "Entry read")
	var resp struct {
		Data mangaBakaEntry `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/my/library/"+url.PathEscape(id), nil, &resp); err != nil {
		if IsNotFound(err) {
			err = nil
		}
		return Entry{}, false, err
	}
	return resp.Data.entry(), true, nil
}

// SaveEntry updates only what u sets, creating the entry when MangaBaka has none.
func (c *MangaBakaClient) SaveEntry(ctx context.Context, id string, u EntryUpdate) error {
	ctx = asCall(ctx, "Entry write")
	body := map[string]any{}
	for state, status := range mangaBakaStates {
		if status == u.Status && state != "considering" {
			body["state"] = state
		}
	}
	if u.Progress >= 0 {
		body["progress_chapter"] = u.Progress
	}
	switch {
	case u.Score > 0:
		body["rating"] = u.Score * 10
	case u.Score == ScoreClear:
		body["rating"] = nil
	}
	if !u.Started.IsZero() {
		body["start_date"] = u.Started.Format(time.DateOnly)
	}
	if !u.Finished.IsZero() {
		body["finish_date"] = u.Finished.Format(time.DateOnly)
	}
	path := "/my/library/" + url.PathEscape(id)
	err := c.do(ctx, http.MethodPatch, path, body, nil)
	if !IsNotFound(err) {
		return err
	}
	if body["state"] == nil {
		body["state"] = "plan_to_read"
		if u.Progress > 0 {
			body["state"] = "reading"
		}
	}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

func (c *MangaBakaClient) DeleteEntry(ctx context.Context, id string) error {
	ctx = asCall(ctx, "Entry delete")
	if err := c.do(ctx, http.MethodDelete, "/my/library/"+url.PathEscape(id), nil, nil); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
