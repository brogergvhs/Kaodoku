package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (c *AniListClient) AuthorizeURL(app App, state, _ string) string {
	q := url.Values{"client_id": {app.ClientID}, "redirect_uri": {app.RedirectURI}, "response_type": {"code"}, "state": {state}}
	return "https://anilist.co/api/v2/oauth/authorize?" + q.Encode()
}

// Exchange swaps an authorization code for a long-lived token.
func (c *AniListClient) Exchange(ctx context.Context, app App, code, _ string) (Token, error) {
	ctx = asCall(ctx, "Sign-in")
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     app.ClientID,
		"client_secret": app.ClientSecret,
		"redirect_uri":  app.RedirectURI,
		"code":          code,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.count(ctx)
	resp, err := c.client.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("anilist token exchange failed (%d): %s", resp.StatusCode, data)
	}
	var out oauthToken
	if err := json.Unmarshal(data, &out); err != nil || out.AccessToken == "" {
		return Token{}, fmt.Errorf("anilist token exchange: unexpected response")
	}
	return Token{Access: out.AccessToken, ExpiresIn: out.ExpiresIn}, nil
}

func (c *AniListClient) Viewer(ctx context.Context) (string, string, error) {
	ctx = asCall(ctx, "Account")
	var out struct {
		Data struct {
			Viewer struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"Viewer"`
		} `json:"data"`
	}
	if err := c.do(ctx, `query { Viewer { id name } }`, nil, &out); err != nil {
		return "", "", err
	}
	if out.Data.Viewer.ID == 0 {
		return "", "", fmt.Errorf("anilist viewer lookup failed")
	}
	return strconv.Itoa(out.Data.Viewer.ID), out.Data.Viewer.Name, nil
}

func (c *AniListClient) viewerID(ctx context.Context) int {
	n, _ := strconv.Atoi(AccountFrom(ctx).UserID)
	return n
}

// ListEntries returns the account's AniList manga list.
func (c *AniListClient) ListEntries(ctx context.Context) ([]Entry, error) {
	ctx = asCall(ctx, "List")
	var resp struct {
		Data struct {
			MediaListCollection struct {
				Lists []struct {
					Entries []struct {
						anilistEntry
						Media anilistMedia `json:"media"`
					} `json:"entries"`
				} `json:"lists"`
			} `json:"MediaListCollection"`
		} `json:"data"`
	}
	q := fmt.Sprintf(`query ($userId: Int) { MediaListCollection(userId: $userId, type: MANGA) { lists { entries { `+entryFields+` media { %s } } } } }`, mediaCore)
	if err := c.do(ctx, q, map[string]any{"userId": c.viewerID(ctx)}, &resp); err != nil {
		return nil, err
	}
	var out []Entry
	seen := map[string]bool{}
	for _, list := range resp.Data.MediaListCollection.Lists {
		for _, e := range list.Entries {
			items, err := anilistMediaToManga([]anilistMedia{e.Media})
			if err != nil || len(items) == 0 {
				continue
			}
			m := items[0]
			if m.Format != "MANGA" || seen[m.ProviderID] {
				continue
			}
			seen[m.ProviderID] = true
			entry := e.entry()
			entry.Manga = m
			out = append(out, entry)
		}
	}
	return out, nil
}

// Favourites returns the AniList ids of the account's favourite manga.
func (c *AniListClient) Favourites(ctx context.Context) ([]string, error) {
	ctx = asCall(ctx, "Favourites")
	var out []string
	for page := 1; page <= 20; page++ {
		var resp struct {
			Data struct {
				User struct {
					Favourites struct {
						Manga struct {
							Nodes []struct {
								ID int `json:"id"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool `json:"hasNextPage"`
							} `json:"pageInfo"`
						} `json:"manga"`
					} `json:"favourites"`
				} `json:"User"`
			} `json:"data"`
		}
		if err := c.do(ctx, `
			query ($id: Int, $page: Int) {
				User(id: $id) { favourites { manga(page: $page, perPage: 50) { nodes { id } pageInfo { hasNextPage } } } }
			}`, map[string]any{"id": c.viewerID(ctx), "page": page}, &resp); err != nil {
			return nil, err
		}
		for _, n := range resp.Data.User.Favourites.Manga.Nodes {
			out = append(out, strconv.Itoa(n.ID))
		}
		if !resp.Data.User.Favourites.Manga.PageInfo.HasNextPage {
			break
		}
	}
	return out, nil
}

func (c *AniListClient) IsFavourite(ctx context.Context, id string) (bool, error) {
	ctx = asCall(ctx, "Favourite check")
	mediaID, err := anilistID(id)
	if err != nil {
		return false, err
	}
	var resp struct {
		Data struct {
			Media struct {
				IsFavourite bool `json:"isFavourite"`
			} `json:"Media"`
		} `json:"data"`
	}
	if err := c.do(ctx, `query ($id: Int) { Media(id: $id, type: MANGA) { isFavourite } }`, map[string]any{"id": mediaID}, &resp); err != nil {
		return false, err
	}
	return resp.Data.Media.IsFavourite, nil
}

// ToggleFavourite flips the favourite state (AniList only offers a toggle).
func (c *AniListClient) ToggleFavourite(ctx context.Context, id string) error {
	ctx = asCall(ctx, "Favourite toggle")
	mediaID, err := anilistID(id)
	if err != nil {
		return err
	}
	var resp struct{}
	return c.do(ctx, `mutation ($id: Int) { ToggleFavourite(mangaId: $id) { manga { nodes { id } } } }`, map[string]any{"id": mediaID}, &resp)
}

func (c *AniListClient) GetEntry(ctx context.Context, id string) (Entry, bool, error) {
	ctx = asCall(ctx, "Entry read")
	mediaID, err := anilistID(id)
	if err != nil {
		return Entry{}, false, err
	}
	var resp struct {
		Data struct {
			Media struct {
				MediaListEntry *anilistEntry `json:"mediaListEntry"`
			} `json:"Media"`
		} `json:"data"`
	}
	if err := c.do(ctx, `query ($id: Int) { Media(id: $id, type: MANGA) { mediaListEntry { `+entryFields+` } } }`, map[string]any{"id": mediaID}, &resp); err != nil {
		return Entry{}, false, err
	}
	e := resp.Data.Media.MediaListEntry
	if e == nil {
		return Entry{}, false, nil
	}
	return e.entry(), true, nil
}

// SaveEntry upserts the list entry. An empty status keeps the remote status;
// progress < 0 keeps the remote progress.
func (c *AniListClient) SaveEntry(ctx context.Context, id string, u EntryUpdate) error {
	ctx = asCall(ctx, "Entry write")
	mediaID, err := anilistID(id)
	if err != nil {
		return err
	}
	vars := map[string]any{"id": mediaID}
	if u.Progress >= 0 {
		vars["progress"] = u.Progress
	}
	if u.Status != "" {
		vars["status"] = u.Status
	}
	if !u.Started.IsZero() {
		vars["startedAt"] = toFuzzy(u.Started)
	}
	if !u.Finished.IsZero() {
		vars["completedAt"] = toFuzzy(u.Finished)
	}
	if u.Score != 0 {
		vars["scoreRaw"] = max(u.Score, 0) * 10
	}
	var resp struct{}
	return c.do(ctx, `
		mutation ($id: Int, $progress: Int, $status: MediaListStatus, $scoreRaw: Int, $startedAt: FuzzyDateInput, $completedAt: FuzzyDateInput) {
			SaveMediaListEntry(mediaId: $id, progress: $progress, status: $status, scoreRaw: $scoreRaw, startedAt: $startedAt, completedAt: $completedAt) { id }
		}`, vars, &resp)
}

func (c *AniListClient) TracksDates() bool { return true }

const entryFields = `progress status score(format: POINT_100) startedAt { year month day } completedAt { year month day }`

type fuzzyDate struct {
	Year  *int `json:"year"`
	Month *int `json:"month"`
	Day   *int `json:"day"`
}

func (f fuzzyDate) time() time.Time {
	if f.Year == nil || f.Month == nil || f.Day == nil {
		return time.Time{}
	}
	return time.Date(*f.Year, time.Month(*f.Month), *f.Day, 0, 0, 0, 0, time.Local)
}

func toFuzzy(t time.Time) map[string]int {
	t = t.In(time.Local)
	return map[string]int{"year": t.Year(), "month": int(t.Month()), "day": t.Day()}
}

type anilistEntry struct {
	Progress    int       `json:"progress"`
	Status      string    `json:"status"`
	Score       float64   `json:"score"`
	StartedAt   fuzzyDate `json:"startedAt"`
	CompletedAt fuzzyDate `json:"completedAt"`
}

func (e anilistEntry) entry() Entry {
	return Entry{Progress: e.Progress, Status: e.Status, Score: roundScore(e.Score / 10), Started: e.StartedAt.time(), Finished: e.CompletedAt.time()}
}

// DeleteEntry removes the list entry entirely. The entry id is resolved via
// the viewer-scoped MediaList query rather than Media.mediaListEntry, which
// AniList serves from a CDN cache that can return a stale or foreign entry id.
func (c *AniListClient) DeleteEntry(ctx context.Context, id string) error {
	ctx = asCall(ctx, "Entry delete")
	mediaID, err := anilistID(id)
	if err != nil {
		return err
	}
	var lookup struct {
		Data struct {
			MediaList *struct {
				ID int `json:"id"`
			} `json:"MediaList"`
		} `json:"data"`
	}
	if err := c.do(ctx, `
		query ($userId: Int, $mediaId: Int) { MediaList(userId: $userId, mediaId: $mediaId, type: MANGA) { id } }
	`, map[string]any{"userId": c.viewerID(ctx), "mediaId": mediaID}, &lookup); err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	if lookup.Data.MediaList == nil {
		return nil
	}
	var resp struct{}
	return c.do(ctx, `mutation ($id: Int) { DeleteMediaListEntry(id: $id) { deleted } }`, map[string]any{"id": lookup.Data.MediaList.ID}, &resp)
}
