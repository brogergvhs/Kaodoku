package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/brogergvhs/kaodoku/internal/util"
)

const AniListProvider = "anilist"

// errAniListNotFound marks an HTTP 404 from AniList, so a delete of an
// already-absent entry can be treated as success.
var errAniListNotFound = errors.New("anilist: not found")

// IsNotFound reports whether err is an AniList 404 (entry deleted upstream).
func IsNotFound(err error) bool { return errors.Is(err, errAniListNotFound) }

// ErrAniListUnauthorized marks an expired or revoked AniList token.
var ErrAniListUnauthorized = errors.New("anilist: unauthorized")

// IsUnauthorized reports whether err means the stored AniList token is invalid.
func IsUnauthorized(err error) bool { return errors.Is(err, ErrAniListUnauthorized) }

// AniListClient queries AniList GraphQL.
type AniListClient struct {
	endpoint string
	client   *http.Client
	limiter  *rate.Limiter
}

// NewAniListClient creates an AniList client.
func NewAniListClient(client *http.Client) *AniListClient {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	// AniList allows 90 req/min but runs degraded at 30/min.
	return &AniListClient{
		endpoint: "https://graphql.anilist.co",
		client:   client,
		limiter:  rate.NewLimiter(rate.Every(2100*time.Millisecond), 5),
	}
}

// SearchFilter narrows an AniList search server-side.
type SearchFilter struct {
	GenreIn    []string
	GenreNotIn []string
	TagIn      []string
	TagNotIn   []string
	Sort       string
	Page       int // 1-based result page; 0 means 1
}

// searchQueryVars builds the GraphQL query and variables for Search.
func searchQueryVars(query string, limit int, f SearchFilter) (string, map[string]any) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	varDefs := "$page: Int, $perPage: Int"
	mediaArgs := "type: MANGA, format: MANGA"
	vars := map[string]any{"page": page, "perPage": limit}
	add := func(name, typ, arg string, val any) {
		varDefs += ", $" + name + ": " + typ
		mediaArgs += ", " + arg + ": $" + name
		vars[name] = val
	}
	if query != "" {
		add("search", "String", "search", query)
	}
	if len(f.GenreIn) > 0 {
		add("genreIn", "[String]", "genre_in", f.GenreIn)
	}
	if len(f.GenreNotIn) > 0 {
		add("genreNotIn", "[String]", "genre_not_in", f.GenreNotIn)
	}
	if len(f.TagIn) > 0 {
		add("tagIn", "[String]", "tag_in", f.TagIn)
	}
	if len(f.TagNotIn) > 0 {
		add("tagNotIn", "[String]", "tag_not_in", f.TagNotIn)
	}
	if len(f.TagIn) > 0 || len(f.TagNotIn) > 0 {
		add("minTagRank", "Int", "minimumTagRank", 0)
	}
	if f.Sort != "" {
		add("sort", "[MediaSort]", "sort", []string{f.Sort})
	}
	gql := fmt.Sprintf(`
		query (%s) {
			Page(page: $page, perPage: $perPage) {
				pageInfo { hasNextPage }
				media(%s) {
					id title { romaji english native } description(asHtml: false)
					coverImage { large } status format chapters volumes synonyms genres averageScore startDate { year } isAdult countryOfOrigin tags { name } staff(sort: RELEVANCE, perPage: 8) { edges { role node { name { full } } } }
				}
			}
		}`, varDefs, mediaArgs)
	return gql, vars
}

const anilistMaxPageEntries = 5000

// searchHasMore combines AniList's hasNextPage with the depth cap.
func searchHasMore(hasNext bool, page, limit int) bool {
	if page < 1 {
		page = 1
	}
	return hasNext && (page+1)*limit <= anilistMaxPageEntries
}

// Search returns one result page and whether further pages exist.
func (c *AniListClient) Search(ctx context.Context, query string, limit int, filter SearchFilter) ([]Manga, bool, error) {
	if limit <= 0 {
		limit = 10
	}
	gql, vars := searchQueryVars(query, limit, filter)
	var resp anilistSearchResponse
	if err := c.do(ctx, gql, vars, &resp); err != nil {
		return nil, false, err
	}
	items, err := anilistMediaToManga(resp.Data.Page.Media)
	return items, searchHasMore(resp.Data.Page.PageInfo.HasNextPage, filter.Page, limit), err
}

// Related returns manga related to an AniList entry (relations first, then
// community recommendations), deduped.
func (c *AniListClient) Related(ctx context.Context, id int, limit int) ([]Manga, error) {
	if limit <= 0 {
		limit = 12
	}
	var resp struct {
		Data struct {
			Media struct {
				Relations struct {
					Edges []struct {
						Node anilistMedia `json:"node"`
					} `json:"edges"`
				} `json:"relations"`
				Recommendations struct {
					Nodes []struct {
						MediaRecommendation anilistMedia `json:"mediaRecommendation"`
					} `json:"nodes"`
				} `json:"recommendations"`
			} `json:"Media"`
		} `json:"data"`
	}
	if err := c.do(ctx, `
		query ($id: Int) {
			Media(id: $id, type: MANGA) {
				relations { edges { node {
					id type title { romaji english native } description(asHtml: false)
					coverImage { large } status format chapters volumes synonyms genres averageScore startDate { year } isAdult countryOfOrigin tags { name }
				} } }
				recommendations(perPage: 12, sort: RATING_DESC) { nodes { mediaRecommendation {
					id type title { romaji english native } description(asHtml: false)
					coverImage { large } status format chapters volumes synonyms genres averageScore startDate { year } isAdult countryOfOrigin tags { name }
				} } }
			}
		}`, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	var media []anilistMedia
	for _, e := range resp.Data.Media.Relations.Edges {
		media = append(media, e.Node)
	}
	for _, n := range resp.Data.Media.Recommendations.Nodes {
		media = append(media, n.MediaRecommendation)
	}
	items, err := anilistMediaToManga(media)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := items[:0]
	for _, m := range items {
		if m.Format != "MANGA" || seen[m.ProviderID] {
			continue
		}
		seen[m.ProviderID] = true
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Trending returns currently trending manga.
func (c *AniListClient) Trending(ctx context.Context, limit int) ([]Manga, error) {
	if limit <= 0 {
		limit = 12
	}
	var resp anilistSearchResponse
	if err := c.do(ctx, `
		query ($perPage: Int) {
			Page(page: 1, perPage: $perPage) {
				media(type: MANGA, format: MANGA, sort: TRENDING_DESC) {
					id title { romaji english native } description(asHtml: false)
					coverImage { large } status format chapters volumes synonyms genres averageScore startDate { year } isAdult countryOfOrigin tags { name } staff(sort: RELEVANCE, perPage: 8) { edges { role node { name { full } } } }
				}
			}
		}`, map[string]any{"perPage": limit}, &resp); err != nil {
		return nil, err
	}
	return anilistMediaToManga(resp.Data.Page.Media)
}

const mediaFields = `id title { romaji english native } description(asHtml: false)
	coverImage { large } status format chapters volumes synonyms genres averageScore startDate { year } isAdult countryOfOrigin tags { name } staff(sort: RELEVANCE, perPage: 8) { edges { role node { name { full } } } }`

// Get returns one AniList manga by ID.
func (c *AniListClient) Get(ctx context.Context, id int) (Manga, error) {
	var resp anilistGetResponse
	if err := c.do(ctx, fmt.Sprintf(`query ($id: Int) { Media(id: $id, type: MANGA) { %s } }`, mediaFields), map[string]any{"id": id}, &resp); err != nil {
		return Manga{}, err
	}
	items, err := anilistMediaToManga([]anilistMedia{resp.Data.Media})
	if err != nil {
		return Manga{}, err
	}
	if len(items) == 0 {
		return Manga{}, fmt.Errorf("anilist manga %d not found", id)
	}
	return items[0], nil
}

// GetWithRelations returns a manga plus its same-collection relations to other
// manga in one request, so a catalog refresh needs no extra round-trip.
func (c *AniListClient) GetWithRelations(ctx context.Context, id int) (Manga, []Relation, error) {
	var resp struct {
		Data struct {
			Media struct {
				anilistMedia
				Relations struct {
					Edges []struct {
						RelationType string `json:"relationType"`
						Node         struct {
							ID   int    `json:"id"`
							Type string `json:"type"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"relations"`
			} `json:"Media"`
		} `json:"data"`
	}
	q := fmt.Sprintf(`query ($id: Int) { Media(id: $id, type: MANGA) { %s relations { edges { relationType node { id type } } } } }`, mediaFields)
	if err := c.do(ctx, q, map[string]any{"id": id}, &resp); err != nil {
		return Manga{}, nil, err
	}
	items, err := anilistMediaToManga([]anilistMedia{resp.Data.Media.anilistMedia})
	if err != nil {
		return Manga{}, nil, err
	}
	if len(items) == 0 {
		return Manga{}, nil, fmt.Errorf("anilist manga %d not found", id)
	}
	var rels []Relation
	for _, e := range resp.Data.Media.Relations.Edges {
		if e.Node.Type == "MANGA" && CollectionRelation(e.RelationType) {
			rels = append(rels, Relation{ProviderID: strconv.Itoa(e.Node.ID), Type: e.RelationType})
		}
	}
	return items[0], rels, nil
}

// TagVocabulary returns AniList's global genre and tag name lists.
func (c *AniListClient) TagVocabulary(ctx context.Context) (genres []string, tags []ContentTag, err error) {
	var resp struct {
		Data struct {
			GenreCollection    []string `json:"GenreCollection"`
			MediaTagCollection []struct {
				Name        string `json:"name"`
				Category    string `json:"category"`
				Description string `json:"description"`
				IsAdult     bool   `json:"isAdult"`
			} `json:"MediaTagCollection"`
		} `json:"data"`
	}
	if err := c.do(ctx, `query { GenreCollection MediaTagCollection { name category description isAdult } }`, nil, &resp); err != nil {
		return nil, nil, err
	}
	for _, t := range resp.Data.MediaTagCollection {
		if t.Name != "" {
			tags = append(tags, ContentTag{Name: t.Name, Kind: "tag", Category: t.Category, Description: t.Description, IsAdult: t.IsAdult})
		}
	}
	return resp.Data.GenreCollection, tags, nil
}

// send paces the request through the client limiter and, when AniList still
// answers 429, sits out the advertised Retry-After window before retrying.
func (c *AniListClient) send(ctx context.Context, body []byte) (*http.Response, error) {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", util.PickUserAgent(""))
		if token := TokenFromContext(ctx); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("anilist request: %w", err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		after := anilistRetryAfter(resp)
		resp.Body.Close()
		if attempt >= attempts {
			return nil, fmt.Errorf("anilist rate limited (HTTP 429), retry after %s", after)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(after):
		}
	}
}

// anilistRetryAfter reads the 429 Retry-After header; AniList's window is one
// minute, so default to that and cap slightly above it.
func anilistRetryAfter(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return time.Minute
	}
	return min(time.Duration(seconds)*time.Second, 90*time.Second)
}

func (c *AniListClient) do(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	resp, err := c.send(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("anilist HTTP 404: %w", errAniListNotFound)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("anilist HTTP 401: %w", ErrAniListUnauthorized)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("anilist HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read anilist response: %w", err)
	}
	// GraphQL failures come back as HTTP 200 with an errors array.
	var failure struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &failure); err == nil && len(failure.Errors) > 0 {
		msg := failure.Errors[0].Message
		if aniListUnauthorizedMessage(msg) {
			return fmt.Errorf("anilist: %s: %w", msg, ErrAniListUnauthorized)
		}
		return fmt.Errorf("anilist: %s", msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode anilist response: %w", err)
	}
	return nil
}

func aniListUnauthorizedMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "invalid token") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "not authenticated")
}

type anilistSearchResponse struct {
	Data struct {
		Page struct {
			PageInfo struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pageInfo"`
			Media []anilistMedia `json:"media"`
		} `json:"Page"`
	} `json:"data"`
}

type anilistGetResponse struct {
	Data struct {
		Media anilistMedia `json:"Media"`
	} `json:"data"`
}

type anilistMedia struct {
	ID    int `json:"id"`
	Title struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	CountryOfOrigin string `json:"countryOfOrigin,omitempty"`
	Description     string `json:"description"`
	CoverImage      struct {
		Large string `json:"large"`
	} `json:"coverImage"`
	Status       string   `json:"status"`
	Format       string   `json:"format"`
	Chapters     *int     `json:"chapters"`
	Volumes      *int     `json:"volumes"`
	Synonyms     []string `json:"synonyms"`
	Genres       []string `json:"genres"`
	AverageScore *int     `json:"averageScore"`
	IsAdult      bool     `json:"isAdult"`
	Tags         []struct {
		Name string `json:"name"`
	} `json:"tags"`
	StartDate struct {
		Year *int `json:"year"`
	} `json:"startDate"`
	Staff struct {
		Edges []struct {
			Role string `json:"role"`
			Node struct {
				Name struct {
					Full string `json:"full"`
				} `json:"name"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"staff"`
}

func anilistMediaToManga(media []anilistMedia) ([]Manga, error) {
	out := make([]Manga, 0, len(media))
	for _, item := range media {
		if item.ID == 0 {
			continue
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		out = append(out, Manga{
			Provider:     AniListProvider,
			ProviderID:   strconv.Itoa(item.ID),
			TitleRomaji:  item.Title.Romaji,
			TitleEnglish: item.Title.English,
			TitleNative:  item.Title.Native,
			Description:  item.Description,
			CoverImage:   item.CoverImage.Large,
			Status:       item.Status,
			Format:       item.Format,
			Chapters:     item.Chapters,
			Volumes:      item.Volumes,
			Synonyms:     item.Synonyms,
			Genres:       item.Genres,
			Authors:      anilistAuthors(item),
			Year:         intOrZero(item.StartDate.Year),
			AverageScore: intOrZero(item.AverageScore),
			IsAdult:      item.IsAdult,
			Tags:         anilistTags(item),
			RawJSON:      string(raw),
		})
	}
	return out, nil
}

func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// anilistAuthors collects the Story/Art credits from the staff edges.
func anilistTags(item anilistMedia) []string {
	out := make([]string, 0, len(item.Tags))
	for _, t := range item.Tags {
		if t.Name != "" {
			out = append(out, t.Name)
		}
	}
	return out
}

func anilistAuthors(m anilistMedia) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range m.Staff.Edges {
		role := strings.ToLower(e.Role)
		if !strings.Contains(role, "story") && !strings.Contains(role, "art") {
			continue
		}
		name := strings.TrimSpace(e.Node.Name.Full)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
