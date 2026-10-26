package catalog

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const malFields = "id,title,main_picture,alternative_titles,start_date,synopsis,mean,genres,media_type,status,num_volumes,num_chapters,authors{first_name,last_name},nsfw"

type MALClient struct {
	restClient
	authBase string
	clientID atomic.Value
}

func NewMALClient(client *http.Client) *MALClient {
	c := &MALClient{restClient: newRest("https://api.myanimelist.net/v2", "myanimelist", client, time.Second), authBase: "https://myanimelist.net/v1/oauth2"}
	c.clientID.Store("")
	c.auth = func(ctx context.Context, h http.Header) {
		if t := AccountFrom(ctx).Token; t != "" {
			h.Set("Authorization", "Bearer "+t)
			return
		}
		h.Set("X-MAL-CLIENT-ID", c.clientID.Load().(string))
	}
	return c
}

func (c *MALClient) SetClientID(id string) { c.clientID.Store(id) }
func (c *MALClient) Name() string          { return MALProvider }
func (c *MALClient) Label() string         { return "MyAnimeList" }

type malManga struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	MainPicture struct {
		Large  string `json:"large"`
		Medium string `json:"medium"`
	} `json:"main_picture"`
	AlternativeTitles struct {
		Synonyms []string `json:"synonyms"`
		En       string   `json:"en"`
		Ja       string   `json:"ja"`
	} `json:"alternative_titles"`
	StartDate string  `json:"start_date"`
	Synopsis  string  `json:"synopsis"`
	Mean      float64 `json:"mean"`
	Genres    []struct {
		Name string `json:"name"`
	} `json:"genres"`
	MediaType   string `json:"media_type"`
	Status      string `json:"status"`
	NumVolumes  int    `json:"num_volumes"`
	NumChapters int    `json:"num_chapters"`
	Authors     []struct {
		Node struct {
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
		} `json:"node"`
	} `json:"authors"`
	NSFW         string `json:"nsfw"`
	RelatedManga []struct {
		Node         malManga `json:"node"`
		RelationType string   `json:"relation_type"`
	} `json:"related_manga"`
	Recommendations []struct {
		Node malManga `json:"node"`
	} `json:"recommendations"`
	MyListStatus *malListStatus `json:"my_list_status"`
}

type malListStatus struct {
	Status          string `json:"status"`
	NumChaptersRead int    `json:"num_chapters_read"`
	IsRereading     bool   `json:"is_rereading"`
	Score           int    `json:"score"`
	StartDate       string `json:"start_date"`
	FinishDate      string `json:"finish_date"`
}

func (s malListStatus) entry() Entry {
	return Entry{Progress: s.NumChaptersRead, Status: malEntryStatus(s), Score: s.Score, Started: malDate(s.StartDate), Finished: malDate(s.FinishDate)}
}

func malDate(v string) time.Time {
	t, _ := time.ParseInLocation(time.DateOnly, v, time.Local)
	return t
}

func (c *MALClient) TracksDates() bool { return true }

type malPage struct {
	Data []struct {
		Node       malManga      `json:"node"`
		ListStatus malListStatus `json:"list_status"`
	} `json:"data"`
	Paging struct {
		Next string `json:"next"`
	} `json:"paging"`
}

func (m malManga) manga() Manga {
	out := Manga{
		Provider:     MALProvider,
		ProviderID:   strconv.Itoa(m.ID),
		TitleRomaji:  m.Title,
		TitleEnglish: m.AlternativeTitles.En,
		TitleNative:  m.AlternativeTitles.Ja,
		Description:  m.Synopsis,
		CoverImage:   m.MainPicture.Large,
		Status:       map[string]string{"finished": "FINISHED", "currently_publishing": "RELEASING", "not_yet_published": "NOT_YET_RELEASED", "on_hiatus": "HIATUS", "discontinued": "CANCELLED"}[m.Status],
		Format:       normFormat(m.MediaType),
		Chapters:     intPtr(m.NumChapters),
		Volumes:      intPtr(m.NumVolumes),
		Synonyms:     m.AlternativeTitles.Synonyms,
		Year:         yearOf(m.StartDate),
		AverageScore: int(m.Mean * 10),
		IsAdult:      m.NSFW != "white",
		RawJSON:      countryRaw(m.MediaType),
	}
	if out.CoverImage == "" {
		out.CoverImage = m.MainPicture.Medium
	}
	for _, g := range m.Genres {
		out.Genres = append(out.Genres, g.Name)
		if g.Name == "Hentai" || g.Name == "Erotica" {
			out.IsAdult = true
		}
	}
	for _, a := range m.Authors {
		if name := strings.TrimSpace(a.Node.FirstName + " " + a.Node.LastName); name != "" {
			out.Authors = append(out.Authors, name)
		}
	}
	return out
}

func (c *MALClient) page(ctx context.Context, path string, q url.Values) ([]Manga, bool, error) {
	var resp malPage
	if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &resp); err != nil {
		return nil, false, err
	}
	out := make([]Manga, 0, len(resp.Data))
	for _, d := range resp.Data {
		out = append(out, d.Node.manga())
	}
	return out, resp.Paging.Next != "", nil
}

func (c *MALClient) Search(ctx context.Context, query string, limit int, f SearchFilter) ([]Manga, bool, error) {
	ctx = asCall(ctx, "Search")
	if limit <= 0 {
		limit = 10
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(max(f.Page-1, 0) * limit)}, "fields": {malFields}, "nsfw": {"true"}}
	if len([]rune(strings.TrimSpace(query))) < 3 {
		q.Set("ranking_type", "bypopularity")
		return c.page(ctx, "/manga/ranking", q)
	}
	q.Set("q", query)
	return c.page(ctx, "/manga", q)
}

// malGenres and malTags are MyAnimeList's fixed manga lists; the official API has no endpoint for them.
var malGenres = []string{"Action", "Adventure", "Avant Garde", "Award Winning", "Boys Love", "Comedy", "Drama", "Ecchi", "Erotica", "Fantasy",
	"Girls Love", "Gourmet", "Hentai", "Horror", "Mystery", "Romance", "Sci-Fi", "Slice of Life", "Sports", "Supernatural", "Suspense"}

var malTags = map[string][]string{
	"Theme": {"Adult Cast", "Anthropomorphic", "CGDCT", "Childcare", "Combat Sports", "Crossdressing", "Delinquents", "Detective", "Educational",
		"Gag Humor", "Gore", "Harem", "High Stakes Game", "Historical", "Idols (Female)", "Idols (Male)", "Isekai", "Iyashikei", "Love Polygon",
		"Love Status Quo", "Magical Sex Shift", "Mahou Shoujo", "Martial Arts", "Mecha", "Medical", "Memoir", "Military", "Music", "Mythology",
		"Organized Crime", "Otaku Culture", "Parody", "Performing Arts", "Pets", "Psychological", "Racing", "Reincarnation", "Reverse Harem",
		"Samurai", "School", "Showbiz", "Space", "Strategy Game", "Super Power", "Survival", "Team Sports", "Time Travel", "Urban Fantasy",
		"Vampire", "Video Game", "Villainess", "Visual Arts", "Workplace"},
	"Demographic": {"Josei", "Kids", "Seinen", "Shoujo", "Shounen"},
}

func (c *MALClient) TagVocabulary(context.Context) ([]ContentTag, error) {
	var out []ContentTag
	for _, n := range malGenres {
		out = append(out, ContentTag{Name: n, Kind: "genre"})
	}
	for category, names := range malTags {
		for _, n := range names {
			out = append(out, ContentTag{Name: n, Kind: "tag", Category: category})
		}
	}
	return out, nil
}

func (c *MALClient) Trending(ctx context.Context, limit int) ([]Manga, error) {
	ctx = asCall(ctx, "Trending")
	items, _, err := c.Search(ctx, "", limit, SearchFilter{})
	return items, err
}

func (c *MALClient) detail(ctx context.Context, id string) (malManga, error) {
	var m malManga
	err := c.do(ctx, http.MethodGet, "/manga/"+url.PathEscape(id)+"?fields="+url.QueryEscape(malFields+",related_manga,recommendations"), nil, &m)
	return m, err
}

func (c *MALClient) Get(ctx context.Context, id string) (Manga, []Relation, error) {
	ctx = asCall(ctx, "Details")
	m, err := c.detail(ctx, id)
	if err != nil {
		return Manga{}, nil, err
	}
	var rels []Relation
	for _, r := range m.RelatedManga {
		if t := normRelation(r.RelationType); CollectionRelation(t) {
			rels = append(rels, Relation{ProviderID: strconv.Itoa(r.Node.ID), Type: t})
		}
	}
	return m.manga(), rels, nil
}

func (c *MALClient) Related(ctx context.Context, id string, limit int) (relations, recommendations []Manga, err error) {
	ctx = asCall(ctx, "Related")
	m, err := c.detail(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range m.RelatedManga {
		rel := r.Node.manga()
		rel.RelationType = normRelation(r.RelationType)
		relations = append(relations, rel)
	}
	for _, r := range m.Recommendations {
		if len(recommendations) < limit || limit <= 0 {
			recommendations = append(recommendations, r.Node.manga())
		}
	}
	return relations, recommendations, nil
}

func (c *MALClient) Viewer(ctx context.Context) (string, string, error) {
	ctx = asCall(ctx, "Account")
	var u struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/@me", nil, &u); err != nil {
		return "", "", err
	}
	return strconv.Itoa(u.ID), u.Name, nil
}

var malToStatus = map[string]string{"reading": "CURRENT", "completed": "COMPLETED", "on_hold": "PAUSED", "dropped": "DROPPED", "plan_to_read": "PLANNING"}

func malEntryStatus(s malListStatus) string {
	if s.IsRereading {
		return "REPEATING"
	}
	return malToStatus[s.Status]
}

func (c *MALClient) ListEntries(ctx context.Context) ([]Entry, error) {
	ctx = asCall(ctx, "List")
	var out []Entry
	next := "/users/@me/mangalist?" + url.Values{"fields": {"list_status," + malFields}, "limit": {"1000"}, "nsfw": {"true"}}.Encode()
	for next != "" {
		var resp malPage
		if err := c.do(ctx, http.MethodGet, next, nil, &resp); err != nil {
			return nil, err
		}
		for _, d := range resp.Data {
			e := d.ListStatus.entry()
			e.Manga = d.Node.manga()
			out = append(out, e)
		}
		next = resp.Paging.Next
	}
	return out, nil
}

func (c *MALClient) GetEntry(ctx context.Context, id string) (Entry, bool, error) {
	ctx = asCall(ctx, "Entry read")
	var m malManga
	if err := c.do(ctx, http.MethodGet, "/manga/"+url.PathEscape(id)+"?fields=my_list_status", nil, &m); err != nil {
		return Entry{}, false, err
	}
	if m.MyListStatus == nil {
		return Entry{}, false, nil
	}
	return m.MyListStatus.entry(), true, nil
}

func (c *MALClient) SaveEntry(ctx context.Context, id string, u EntryUpdate) error {
	ctx = asCall(ctx, "Entry write")
	form := url.Values{}
	if u.Progress >= 0 {
		form.Set("num_chapters_read", strconv.Itoa(u.Progress))
	}
	if !u.Started.IsZero() {
		form.Set("start_date", u.Started.In(time.Local).Format(time.DateOnly))
	}
	if !u.Finished.IsZero() {
		form.Set("finish_date", u.Finished.In(time.Local).Format(time.DateOnly))
	}
	if u.Score != 0 {
		form.Set("score", strconv.Itoa(max(u.Score, 0)))
	}
	switch status := u.Status; status {
	case "":
	case "REPEATING":
		form.Set("status", "completed")
		form.Set("is_rereading", "true")
	default:
		for k, v := range malToStatus {
			if v == status {
				form.Set("status", k)
			}
		}
	}
	if len(form) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPatch, "/manga/"+url.PathEscape(id)+"/my_list_status", form, nil)
}

func (c *MALClient) DeleteEntry(ctx context.Context, id string) error {
	ctx = asCall(ctx, "Entry delete")
	if err := c.do(ctx, http.MethodDelete, "/manga/"+url.PathEscape(id)+"/my_list_status", nil, nil); err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// AuthorizeURL omits redirect_uri: MAL then uses the app's single registered URL, avoiding scheme mismatches behind proxies.
func (c *MALClient) AuthorizeURL(app App, state, verifier string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {app.ClientID}, "code_challenge": {verifier}, "code_challenge_method": {"plain"}, "state": {state}}
	return c.authBase + "/authorize?" + q.Encode()
}

func (c *MALClient) Exchange(ctx context.Context, app App, code, verifier string) (Token, error) {
	ctx = asCall(ctx, "Sign-in")
	form := url.Values{"client_id": {app.ClientID}, "grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}}
	if app.ClientSecret != "" {
		form.Set("client_secret", app.ClientSecret)
	}
	return c.token(ctx, c.authBase+"/token", form)
}

func (c *MALClient) Refresh(ctx context.Context, app App, refresh string) (Token, error) {
	ctx = asCall(ctx, "Token refresh")
	form := url.Values{"client_id": {app.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	if app.ClientSecret != "" {
		form.Set("client_secret", app.ClientSecret)
	}
	return c.token(ctx, c.authBase+"/token", form)
}
