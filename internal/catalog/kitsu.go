package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type KitsuClient struct {
	restClient
	tokenURL string
}

func NewKitsuClient(client *http.Client) *KitsuClient {
	c := &KitsuClient{restClient: newRest("https://kitsu.app/api/edge", "kitsu", client, 500*time.Millisecond), tokenURL: "https://kitsu.app/api/oauth/token"}
	c.contentType = "application/vnd.api+json"
	c.auth = bearer
	return c
}

func (c *KitsuClient) Name() string  { return KitsuProvider }
func (c *KitsuClient) Label() string { return "Kitsu" }

type kitsuRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type kitsuResource struct {
	kitsuRef
	Attributes    json.RawMessage `json:"attributes"`
	Relationships map[string]struct {
		Data json.RawMessage `json:"data"`
	} `json:"relationships"`
}

type kitsuDoc struct {
	Data     json.RawMessage `json:"data"`
	Included []kitsuResource `json:"included"`
	Links    struct {
		Next string `json:"next"`
	} `json:"links"`
}

func (d kitsuDoc) resources() []kitsuResource {
	var many []kitsuResource
	if json.Unmarshal(d.Data, &many) == nil {
		return many
	}
	var one kitsuResource
	if json.Unmarshal(d.Data, &one) == nil && one.ID != "" {
		return []kitsuResource{one}
	}
	return nil
}

func (d kitsuDoc) find(ref kitsuRef) (kitsuResource, bool) {
	for _, r := range d.Included {
		if r.kitsuRef == ref {
			return r, true
		}
	}
	return kitsuResource{}, false
}

func (r kitsuResource) refs(name string) []kitsuRef {
	raw := r.Relationships[name].Data
	var many []kitsuRef
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one kitsuRef
	if json.Unmarshal(raw, &one) == nil && one.ID != "" {
		return []kitsuRef{one}
	}
	return nil
}

type kitsuMangaAttrs struct {
	CanonicalTitle    string            `json:"canonicalTitle"`
	Titles            map[string]string `json:"titles"`
	AbbreviatedTitles []string          `json:"abbreviatedTitles"`
	Synopsis          string            `json:"synopsis"`
	PosterImage       *struct {
		Large    string `json:"large"`
		Original string `json:"original"`
	} `json:"posterImage"`
	CoverImage *struct {
		Large string `json:"large"`
	} `json:"coverImage"`
	Status        string `json:"status"`
	Subtype       string `json:"subtype"`
	ChapterCount  int    `json:"chapterCount"`
	VolumeCount   int    `json:"volumeCount"`
	StartDate     string `json:"startDate"`
	AverageRating string `json:"averageRating"`
	AgeRating     string `json:"ageRating"`
}

// manga treats a null ageRating as adult so the content guard fails closed.
func (d kitsuDoc) manga(r kitsuResource) Manga {
	var a kitsuMangaAttrs
	_ = json.Unmarshal(r.Attributes, &a)
	rating, _ := strconv.ParseFloat(a.AverageRating, 64)
	m := Manga{
		Provider:     KitsuProvider,
		ProviderID:   r.ID,
		TitleRomaji:  firstNonEmpty(a.Titles["en_jp"], a.CanonicalTitle),
		TitleEnglish: firstNonEmpty(a.Titles["en"], a.Titles["en_us"]),
		TitleNative:  firstNonEmpty(a.Titles["ja_jp"], a.Titles["ko_kr"], a.Titles["zh_cn"]),
		Description:  a.Synopsis,
		Status:       map[string]string{"current": "RELEASING", "finished": "FINISHED", "tba": "NOT_YET_RELEASED", "unreleased": "NOT_YET_RELEASED", "upcoming": "NOT_YET_RELEASED"}[a.Status],
		Format:       normFormat(a.Subtype),
		Chapters:     intPtr(a.ChapterCount),
		Volumes:      intPtr(a.VolumeCount),
		Synonyms:     a.AbbreviatedTitles,
		Year:         yearOf(a.StartDate),
		AverageScore: int(rating),
		IsAdult:      a.AgeRating == "" || a.AgeRating == "R18",
		RawJSON:      countryRaw(a.Subtype),
	}
	if a.PosterImage != nil {
		m.CoverImage = firstNonEmpty(a.PosterImage.Large, a.PosterImage.Original)
	}
	if a.CoverImage != nil {
		m.BannerImage = a.CoverImage.Large
	}
	for _, ref := range r.refs("categories") {
		if cat, ok := d.find(ref); ok {
			var ca kitsuCategory
			if json.Unmarshal(cat.Attributes, &ca) == nil && ca.Title != "" {
				m.Genres = append(m.Genres, ca.Title)
				m.IsAdult = m.IsAdult || ca.NSFW || AdultName(ca.Title)
			}
		}
	}
	for _, ref := range r.refs("mappings") {
		if mp, ok := d.find(ref); ok {
			var ma struct {
				ExternalSite string `json:"externalSite"`
				ExternalID   string `json:"externalId"`
			}
			_ = json.Unmarshal(mp.Attributes, &ma)
			if p := map[string]string{"myanimelist/manga": MALProvider, "anilist/manga": AniListProvider}[ma.ExternalSite]; p != "" {
				if m.ExternalIDs == nil {
					m.ExternalIDs = map[string]string{}
				}
				m.ExternalIDs[p] = ma.ExternalID
			}
		}
	}
	return m
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (c *KitsuClient) list(ctx context.Context, path string, q url.Values) ([]Manga, bool, error) {
	var doc kitsuDoc
	if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &doc); err != nil {
		return nil, false, err
	}
	var out []Manga
	for _, r := range doc.resources() {
		if r.Type == "manga" {
			out = append(out, doc.manga(r))
		}
	}
	return out, doc.Links.Next != "", nil
}

func (c *KitsuClient) Search(ctx context.Context, query string, limit int, f SearchFilter) ([]Manga, bool, error) {
	ctx = asCall(ctx, "Search")
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 20)
	q := url.Values{"page[limit]": {strconv.Itoa(limit)}, "page[offset]": {strconv.Itoa(max(f.Page-1, 0) * limit)}, "include": {"categories"}}
	if in := slices.Concat(f.GenreIn, f.TagIn); len(in) > 0 {
		q.Set("filter[categories]", strings.Join(in, ","))
	}
	if query = strings.TrimSpace(query); query != "" {
		q.Set("filter[text]", query)
	} else {
		q.Set("sort", "-userCount")
	}
	return c.list(ctx, "/manga", q)
}

type kitsuCategory struct {
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	NSFW        bool   `json:"nsfw"`
}

// TagVocabulary pages through Kitsu's categories; search filters take their slugs.
func (c *KitsuClient) TagVocabulary(ctx context.Context) ([]ContentTag, error) {
	ctx = asCall(ctx, "Tags")
	var out []ContentTag
	for offset := 0; ; offset += 20 {
		var doc kitsuDoc
		if err := c.do(ctx, http.MethodGet, "/categories?"+url.Values{"page[limit]": {"20"}, "page[offset]": {strconv.Itoa(offset)}}.Encode(), nil, &doc); err != nil {
			return nil, err
		}
		for _, r := range doc.resources() {
			var ca kitsuCategory
			if json.Unmarshal(r.Attributes, &ca) == nil && ca.Title != "" {
				out = append(out, ContentTag{Name: ca.Title, Kind: "genre", Slug: ca.Slug, Description: ca.Description, IsAdult: ca.NSFW})
			}
		}
		if doc.Links.Next == "" {
			return out, nil
		}
	}
}

func (c *KitsuClient) Trending(ctx context.Context, limit int) ([]Manga, error) {
	ctx = asCall(ctx, "Trending")
	items, _, err := c.list(ctx, "/trending/manga", url.Values{"limit": {strconv.Itoa(max(limit, 1))}})
	return items, err
}

func (c *KitsuClient) detail(ctx context.Context, id string) (kitsuDoc, kitsuResource, error) {
	var doc kitsuDoc
	q := url.Values{"include": {"categories,mappings,mediaRelationships.destination"}}
	if err := c.do(ctx, http.MethodGet, "/manga/"+url.PathEscape(id)+"?"+q.Encode(), nil, &doc); err != nil {
		return doc, kitsuResource{}, err
	}
	res := doc.resources()
	if len(res) == 0 {
		return doc, kitsuResource{}, ErrNotFound
	}
	return doc, res[0], nil
}

func (c *KitsuClient) relations(doc kitsuDoc, r kitsuResource) []Manga {
	var out []Manga
	for _, ref := range r.refs("mediaRelationships") {
		rel, ok := doc.find(ref)
		if !ok {
			continue
		}
		var ra struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(rel.Attributes, &ra)
		for _, dst := range rel.refs("destination") {
			if res, ok := doc.find(dst); ok && dst.Type == "manga" {
				m := doc.manga(res)
				m.RelationType = normRelation(ra.Role)
				out = append(out, m)
			}
		}
	}
	return out
}

func (c *KitsuClient) Get(ctx context.Context, id string) (Manga, []Relation, error) {
	ctx = asCall(ctx, "Details")
	doc, r, err := c.detail(ctx, id)
	if err != nil {
		return Manga{}, nil, err
	}
	var rels []Relation
	for _, m := range c.relations(doc, r) {
		if CollectionRelation(m.RelationType) {
			rels = append(rels, Relation{ProviderID: m.ProviderID, Type: m.RelationType})
		}
	}
	return doc.manga(r), rels, nil
}

func (c *KitsuClient) Related(ctx context.Context, id string, _ int) ([]Manga, []Manga, error) {
	ctx = asCall(ctx, "Related")
	doc, r, err := c.detail(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return c.relations(doc, r), nil, nil
}

func (c *KitsuClient) Resolve(ctx context.Context, provider, id string) (string, bool, error) {
	ctx = asCall(ctx, "ID lookup")
	site := map[string]string{MALProvider: "myanimelist/manga", AniListProvider: "anilist/manga"}[provider]
	if site == "" {
		return "", false, nil
	}
	var doc kitsuDoc
	q := url.Values{"filter[externalSite]": {site}, "filter[externalId]": {id}, "include": {"item"}}
	if err := c.do(ctx, http.MethodGet, "/mappings?"+q.Encode(), nil, &doc); err != nil {
		return "", false, err
	}
	for _, r := range doc.resources() {
		for _, item := range r.refs("item") {
			if item.Type == "manga" {
				return item.ID, true, nil
			}
		}
	}
	return "", false, nil
}

func (c *KitsuClient) Viewer(ctx context.Context) (string, string, error) {
	ctx = asCall(ctx, "Account")
	var doc kitsuDoc
	if err := c.do(ctx, http.MethodGet, "/users?filter[self]=true", nil, &doc); err != nil {
		return "", "", err
	}
	for _, r := range doc.resources() {
		var a struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(r.Attributes, &a)
		return r.ID, a.Name, nil
	}
	return "", "", ErrUnauthorized
}

var kitsuToStatus = map[string]string{"current": "CURRENT", "planned": "PLANNING", "completed": "COMPLETED", "on_hold": "PAUSED", "dropped": "DROPPED"}

func (c *KitsuClient) entries(ctx context.Context, q url.Values) ([]Entry, error) {
	q.Set("filter[userId]", AccountFrom(ctx).UserID)
	q.Set("filter[kind]", "manga")
	q.Set("include", "manga")
	q.Set("page[limit]", "500")
	var out []Entry
	next := "/library-entries?" + q.Encode()
	for next != "" {
		var doc kitsuDoc
		if err := c.do(ctx, http.MethodGet, next, nil, &doc); err != nil {
			return nil, err
		}
		for _, r := range doc.resources() {
			var a struct {
				Status      string    `json:"status"`
				Progress    int       `json:"progress"`
				Reconsuming bool      `json:"reconsuming"`
				Rating      *int      `json:"ratingTwenty"`
				StartedAt   time.Time `json:"startedAt"`
				FinishedAt  time.Time `json:"finishedAt"`
			}
			_ = json.Unmarshal(r.Attributes, &a)
			e := Entry{Progress: a.Progress, Status: kitsuToStatus[a.Status], Started: a.StartedAt, Finished: a.FinishedAt, ref: r.ID}
			if a.Rating != nil {
				e.Score = roundScore(float64(*a.Rating) / 2)
			}
			if a.Reconsuming {
				e.Status = "REPEATING"
			}
			for _, ref := range r.refs("manga") {
				if res, ok := doc.find(ref); ok {
					e.Manga = doc.manga(res)
				}
			}
			out = append(out, e)
		}
		next = doc.Links.Next
	}
	return out, nil
}

func (c *KitsuClient) ListEntries(ctx context.Context) ([]Entry, error) {
	ctx = asCall(ctx, "List")
	return c.entries(ctx, url.Values{})
}

func (c *KitsuClient) GetEntry(ctx context.Context, id string) (Entry, bool, error) {
	ctx = asCall(ctx, "Entry read")
	list, err := c.entries(ctx, url.Values{"filter[mangaId]": {id}})
	if err != nil || len(list) == 0 {
		return Entry{}, false, err
	}
	return list[0], true, nil
}

func (c *KitsuClient) TracksDates() bool { return true }

func (c *KitsuClient) SaveEntry(ctx context.Context, id string, u EntryUpdate) error {
	ctx = asCall(ctx, "Entry write")
	progress, status := u.Progress, u.Status
	e, found, err := u.remote(ctx, id, c.GetEntry)
	if err != nil {
		return err
	}
	attrs := map[string]any{}
	if progress >= 0 {
		attrs["progress"] = progress
	}
	if !u.Started.IsZero() {
		attrs["startedAt"] = u.Started.UTC().Format(time.RFC3339)
	}
	if !u.Finished.IsZero() {
		attrs["finishedAt"] = u.Finished.UTC().Format(time.RFC3339)
	}
	if u.Score != 0 {
		attrs["ratingTwenty"] = nil
		if u.Score > 0 {
			attrs["ratingTwenty"] = u.Score * 2
		}
	}
	if status == "REPEATING" {
		attrs["status"], attrs["reconsuming"] = "current", true
	} else if status != "" {
		for k, v := range kitsuToStatus {
			if v == status {
				attrs["status"] = k
			}
		}
	}
	if found && len(attrs) == 0 {
		return nil
	}
	if !found && attrs["status"] == nil {
		attrs["status"] = "planned"
	}
	err = c.writeEntry(ctx, id, e, found, attrs)
	if err == nil || progress <= 0 || !strings.Contains(err.Error(), "cannot exceed length") {
		return err
	}
	_, a, derr := c.detail(ctx, id)
	if derr != nil {
		return err
	}
	var m kitsuMangaAttrs
	_ = json.Unmarshal(a.Attributes, &m)
	if m.ChapterCount <= 0 || m.ChapterCount >= progress {
		return err
	}
	attrs["progress"] = m.ChapterCount
	return c.writeEntry(ctx, id, e, found, attrs)
}

func (c *KitsuClient) writeEntry(ctx context.Context, id string, e Entry, found bool, attrs map[string]any) error {
	if found {
		return c.do(ctx, http.MethodPatch, "/library-entries/"+e.ref, map[string]any{"data": map[string]any{"id": e.ref, "type": "libraryEntries", "attributes": attrs}}, nil)
	}
	body := map[string]any{"data": map[string]any{
		"type":       "libraryEntries",
		"attributes": attrs,
		"relationships": map[string]any{
			"user":  map[string]any{"data": kitsuRef{Type: "users", ID: AccountFrom(ctx).UserID}},
			"manga": map[string]any{"data": kitsuRef{Type: "manga", ID: id}},
		},
	}}
	return c.do(ctx, http.MethodPost, "/library-entries", body, nil)
}

func (c *KitsuClient) DeleteEntry(ctx context.Context, id string) error {
	ctx = asCall(ctx, "Entry delete")
	e, found, err := c.GetEntry(ctx, id)
	if err != nil || !found {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/library-entries/"+e.ref, nil, nil)
}

func (c *KitsuClient) Login(ctx context.Context, username, password string) (Token, error) {
	ctx = asCall(ctx, "Sign-in")
	return c.token(ctx, c.tokenURL, url.Values{"grant_type": {"password"}, "username": {username}, "password": {password}})
}

func (c *KitsuClient) Refresh(ctx context.Context, _ App, refresh string) (Token, error) {
	ctx = asCall(ctx, "Token refresh")
	return c.token(ctx, c.tokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}
