package catalog

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

type MangaUpdatesClient struct{ restClient }

func NewMangaUpdatesClient(client *http.Client) *MangaUpdatesClient {
	c := &MangaUpdatesClient{newRest("https://api.mangaupdates.com/v1", "mangaupdates", client, time.Second)}
	c.limiter = rate.NewLimiter(rate.Every(1100*time.Millisecond), 1)
	c.auth = bearer
	return c
}

func (c *MangaUpdatesClient) Name() string  { return MangaUpdatesProvider }
func (c *MangaUpdatesClient) Label() string { return "MangaUpdates" }

type muImage struct {
	URL struct {
		Original string `json:"original"`
		Thumb    string `json:"thumb"`
	} `json:"url"`
}

type muSeries struct {
	SeriesID   int64  `json:"series_id"`
	Title      string `json:"title"`
	Associated []struct {
		Title string `json:"title"`
	} `json:"associated"`
	Description    string     `json:"description"`
	Image          muImage    `json:"image"`
	Type           string     `json:"type"`
	Year           flexString `json:"year"`
	BayesianRating float64    `json:"bayesian_rating"`
	Genres         []struct {
		Genre string `json:"genre"`
	} `json:"genres"`
	Categories []struct {
		Category string `json:"category"`
	} `json:"categories"`
	Completed     bool `json:"completed"`
	LatestChapter int  `json:"latest_chapter"`
	Authors       []struct {
		Name string `json:"name"`
	} `json:"authors"`
	RelatedSeries []struct {
		RelationType      string `json:"relation_type"`
		RelatedSeriesID   int64  `json:"related_series_id"`
		RelatedSeriesName string `json:"related_series_name"`
	} `json:"related_series"`
	Recommendations []struct {
		SeriesName  string  `json:"series_name"`
		SeriesID    int64   `json:"series_id"`
		SeriesImage muImage `json:"series_image"`
	} `json:"recommendations"`
}

const muRelatedDetails = 12

func (s muSeries) manga() Manga {
	year, _ := strconv.Atoi(string(s.Year))
	m := Manga{
		Provider:     MangaUpdatesProvider,
		ProviderID:   strconv.FormatInt(s.SeriesID, 10),
		TitleRomaji:  s.Title,
		Description:  s.Description,
		CoverImage:   firstNonEmpty(s.Image.URL.Original, s.Image.URL.Thumb),
		Status:       "RELEASING",
		Format:       normFormat(s.Type),
		Year:         year,
		AverageScore: int(s.BayesianRating * 10),
		IsAdult:      len(s.Genres) == 0,
		RawJSON:      countryRaw(s.Type),
	}
	if s.Completed {
		m.Status, m.Chapters = "FINISHED", intPtr(s.LatestChapter)
	}
	for _, a := range s.Associated {
		m.Synonyms = append(m.Synonyms, a.Title)
	}
	for _, g := range s.Genres {
		m.Genres = append(m.Genres, g.Genre)
		if AdultName(g.Genre) {
			m.IsAdult = true
		}
	}
	for _, cat := range s.Categories {
		if len(m.Tags) < 20 {
			m.Tags = append(m.Tags, cat.Category)
		}
	}
	for _, a := range s.Authors {
		m.Authors = append(m.Authors, a.Name)
	}
	return m
}

func (c *MangaUpdatesClient) Search(ctx context.Context, query string, limit int, f SearchFilter) ([]Manga, bool, error) {
	if limit <= 0 {
		limit = 10
	}
	page := max(f.Page, 1)
	body := map[string]any{"page": page, "perpage": limit}
	if query = strings.TrimSpace(query); query != "" {
		body["search"] = query
	} else {
		body["orderby"] = "rating"
	}
	if in := slices.Concat(f.GenreIn, f.TagIn); len(in) > 0 {
		body["genre"] = in
	}
	if out := slices.Concat(f.GenreNotIn, f.TagNotIn); len(out) > 0 {
		body["exclude_genre"] = out
	}
	var resp struct {
		TotalHits int `json:"total_hits"`
		Results   []struct {
			Record muSeries `json:"record"`
		} `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/series/search", body, &resp); err != nil {
		return nil, false, err
	}
	out := make([]Manga, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, r.Record.manga())
	}
	return out, page*limit < resp.TotalHits, nil
}

func (c *MangaUpdatesClient) TagVocabulary(ctx context.Context) ([]ContentTag, error) {
	var genres []struct {
		Genre       string `json:"genre"`
		Description string `json:"description"`
	}
	if err := c.do(ctx, http.MethodGet, "/genres", nil, &genres); err != nil {
		return nil, err
	}
	out := make([]ContentTag, 0, len(genres))
	for _, g := range genres {
		out = append(out, ContentTag{Name: g.Genre, Kind: "genre", Description: g.Description})
	}
	return out, nil
}

func (c *MangaUpdatesClient) Trending(ctx context.Context, limit int) ([]Manga, error) {
	items, _, err := c.Search(ctx, "", limit, SearchFilter{})
	return items, err
}

func (c *MangaUpdatesClient) series(ctx context.Context, id string) (muSeries, error) {
	var s muSeries
	err := c.do(ctx, http.MethodGet, "/series/"+url.PathEscape(id), nil, &s)
	return s, err
}

func (c *MangaUpdatesClient) Get(ctx context.Context, id string) (Manga, []Relation, error) {
	s, err := c.series(ctx, id)
	if err != nil {
		return Manga{}, nil, err
	}
	var rels []Relation
	for _, r := range s.RelatedSeries {
		if t := normRelation(r.RelationType); CollectionRelation(t) {
			rels = append(rels, Relation{ProviderID: strconv.FormatInt(r.RelatedSeriesID, 10), Type: t})
		}
	}
	return s.manga(), rels, nil
}

func (c *MangaUpdatesClient) Related(ctx context.Context, id string, limit int) (relations, recommendations []Manga, err error) {
	s, err := c.series(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	for i, r := range s.RelatedSeries {
		id := strconv.FormatInt(r.RelatedSeriesID, 10)
		m := Manga{Provider: MangaUpdatesProvider, ProviderID: id, TitleRomaji: r.RelatedSeriesName, IsAdult: true}
		if i < muRelatedDetails {
			if full, err := c.series(ctx, id); err == nil {
				m = full.manga()
			}
		}
		m.RelationType = normRelation(r.RelationType)
		relations = append(relations, m)
	}
	return relations, s.recommendations(limit), nil
}

func (c *MangaUpdatesClient) Recommendations(ctx context.Context, id string, limit int) ([]Manga, error) {
	s, err := c.series(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.recommendations(limit), nil
}

// recommendations come without genres, so they stay adult until their details load.
func (s muSeries) recommendations(limit int) []Manga {
	var out []Manga
	for _, r := range s.Recommendations {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, Manga{Provider: MangaUpdatesProvider, ProviderID: strconv.FormatInt(r.SeriesID, 10), TitleRomaji: r.SeriesName, CoverImage: r.SeriesImage.URL.Original, IsAdult: true})
	}
	return out
}

func (c *MangaUpdatesClient) Viewer(ctx context.Context) (string, string, error) {
	var p struct {
		UserID   int64  `json:"user_id"`
		Username string `json:"username"`
	}
	if err := c.do(ctx, http.MethodGet, "/account/profile", nil, &p); err != nil {
		return "", "", err
	}
	return strconv.FormatInt(p.UserID, 10), p.Username, nil
}

var muListStatus = []string{"CURRENT", "PLANNING", "COMPLETED", "DROPPED", "PAUSED"}

type muListEntry struct {
	Series struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"series"`
	ListID int `json:"list_id"`
	Status struct {
		Chapter int `json:"chapter"`
	} `json:"status"`
}

func muStatus(listID int) string {
	if listID >= 0 && listID < len(muListStatus) {
		return muListStatus[listID]
	}
	return "CURRENT"
}

func muListFor(status string, progress int) int {
	for i, s := range muListStatus {
		if s == status {
			return i
		}
	}
	if status == "REPEATING" || progress > 0 {
		return 0
	}
	return 1
}

func (c *MangaUpdatesClient) ListEntries(ctx context.Context) ([]Entry, error) {
	var out []Entry
	for list := range muListStatus {
		for page := 1; page <= 50; page++ {
			var resp struct {
				TotalHits int `json:"total_hits"`
				Results   []struct {
					Record   muListEntry `json:"record"`
					Metadata struct {
						Series muSeries `json:"series"`
					} `json:"metadata"`
				} `json:"results"`
			}
			if err := c.do(ctx, http.MethodPost, "/lists/"+strconv.Itoa(list)+"/search", map[string]any{"page": page, "perpage": 100}, &resp); err != nil {
				return nil, err
			}
			for _, r := range resp.Results {
				m := r.Metadata.Series.manga()
				if m.ProviderID == "0" {
					m = Manga{Provider: MangaUpdatesProvider, ProviderID: strconv.FormatInt(r.Record.Series.ID, 10), TitleRomaji: r.Record.Series.Title, IsAdult: true}
				}
				out = append(out, Entry{Manga: m, Progress: r.Record.Status.Chapter, Status: muStatus(list)})
			}
			if page*100 >= resp.TotalHits {
				break
			}
		}
	}
	return out, nil
}

func (c *MangaUpdatesClient) entry(ctx context.Context, id string) (muListEntry, bool, error) {
	var e muListEntry
	if err := c.do(ctx, http.MethodGet, "/lists/series/"+url.PathEscape(id), nil, &e); err != nil {
		if IsNotFound(err) {
			return e, false, nil
		}
		return e, false, err
	}
	return e, true, nil
}

func (c *MangaUpdatesClient) GetEntry(ctx context.Context, id string) (Entry, bool, error) {
	e, ok, err := c.entry(ctx, id)
	return Entry{Progress: e.Status.Chapter, Status: muStatus(e.ListID)}, ok, err
}

func (c *MangaUpdatesClient) SaveEntry(ctx context.Context, id string, u EntryUpdate) error {
	progress, status := u.Progress, u.Status
	sid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return ErrNotFound
	}
	e, found, err := c.entry(ctx, id)
	if err != nil {
		return err
	}
	list, chapter := muListFor(status, progress), progress
	if found && status == "" {
		list = e.ListID
	}
	if chapter < 0 {
		chapter = e.Status.Chapter
	}
	item := map[string]any{"series": map[string]any{"id": sid}, "list_id": list, "status": map[string]any{"chapter": chapter}}
	path := "/lists/series"
	if found {
		path += "/update"
	}
	return c.do(ctx, http.MethodPost, path, []any{item}, nil)
}

func (c *MangaUpdatesClient) DeleteEntry(ctx context.Context, id string) error {
	sid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil
	}
	if err := c.do(ctx, http.MethodPost, "/lists/series/delete", []int64{sid}, nil); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

func (c *MangaUpdatesClient) Login(ctx context.Context, username, password string) (Token, error) {
	var resp struct {
		Context struct {
			SessionToken string `json:"session_token"`
		} `json:"context"`
	}
	if err := c.do(ctx, http.MethodPut, "/account/login", map[string]string{"username": username, "password": password}, &resp); err != nil {
		return Token{}, err
	}
	if resp.Context.SessionToken == "" {
		return Token{}, ErrUnauthorized
	}
	return Token{Access: resp.Context.SessionToken}, nil
}
