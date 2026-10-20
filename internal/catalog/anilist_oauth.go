package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const anilistTokenURL = "https://anilist.co/api/v2/oauth/token"

type ctxTokenKey struct{}

// WithToken carries a user's AniList access token; the client attaches it to
// requests, raising rate limits and unlocking user-scoped fields.
func WithToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxTokenKey{}, token)
}

// TokenFromContext returns the AniList token carried by the context, if any.
func TokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(ctxTokenKey{}).(string)
	return t
}

// AniListExchangeCode swaps an authorization code for a long-lived token.
func AniListExchangeCode(ctx context.Context, clientID, clientSecret, redirectURI, code string) (token string, expiresIn int, err error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     clientID,
		"client_secret": clientSecret,
		"redirect_uri":  redirectURI,
		"code":          code,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anilistTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("anilist token exchange failed (%d): %s", resp.StatusCode, data)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.AccessToken == "" {
		return "", 0, fmt.Errorf("anilist token exchange: unexpected response")
	}
	return out.AccessToken, out.ExpiresIn, nil
}

// AniListViewer returns the authenticated account's identity.
func AniListViewer(ctx context.Context, token string) (id int, name string, err error) {
	body, _ := json.Marshal(map[string]any{"query": `query { Viewer { id name } }`})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://graphql.anilist.co", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Viewer struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"Viewer"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, "", err
	}
	if out.Data.Viewer.ID == 0 {
		return 0, "", fmt.Errorf("anilist viewer lookup failed")
	}
	return out.Data.Viewer.ID, out.Data.Viewer.Name, nil
}

// AniListEntry is one manga on a user's AniList list.
type AniListEntry struct {
	Manga    Manga
	Progress int    // chapters read on AniList
	Status   string // CURRENT, COMPLETED, PLANNING, ...
}

// UserList returns the user's AniList manga list (authenticated via ctx token).
func (c *AniListClient) UserList(ctx context.Context, anilistUserID int) ([]AniListEntry, error) {
	var resp struct {
		Data struct {
			MediaListCollection struct {
				Lists []struct {
					Entries []struct {
						Progress int          `json:"progress"`
						Status   string       `json:"status"`
						Media    anilistMedia `json:"media"`
					} `json:"entries"`
				} `json:"lists"`
			} `json:"MediaListCollection"`
		} `json:"data"`
	}
	if err := c.do(ctx, `
		query ($userId: Int) {
			MediaListCollection(userId: $userId, type: MANGA) {
				lists { entries { progress status media {
					id title { romaji english native } description(asHtml: false)
					coverImage { large } bannerImage status format chapters volumes synonyms genres averageScore startDate { year } isAdult tags { name }
				} } }
			}
		}`, map[string]any{"userId": anilistUserID}, &resp); err != nil {
		return nil, err
	}
	var out []AniListEntry
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
			out = append(out, AniListEntry{Manga: m, Progress: e.Progress, Status: e.Status})
		}
	}
	return out, nil
}

// FavouriteManga returns the AniList ids of a user's favourite manga.
func (c *AniListClient) FavouriteManga(ctx context.Context, anilistUserID int) ([]int, error) {
	var out []int
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
			}`, map[string]any{"id": anilistUserID, "page": page}, &resp); err != nil {
			return nil, err
		}
		for _, n := range resp.Data.User.Favourites.Manga.Nodes {
			out = append(out, n.ID)
		}
		if !resp.Data.User.Favourites.Manga.PageInfo.HasNextPage {
			break
		}
	}
	return out, nil
}

// IsFavourite reports whether the authenticated user favourited a media.
func (c *AniListClient) IsFavourite(ctx context.Context, mediaID int) (bool, error) {
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

// ToggleFavourite flips the authenticated user's favourite state for a manga
// (AniList only offers a toggle; callers check IsFavourite first).
func (c *AniListClient) ToggleFavourite(ctx context.Context, mediaID int) error {
	var resp struct {
		Data struct {
			ToggleFavourite struct {
				Manga struct {
					Nodes []struct {
						ID int `json:"id"`
					} `json:"nodes"`
				} `json:"manga"`
			} `json:"ToggleFavourite"`
		} `json:"data"`
	}
	return c.do(ctx, `mutation ($id: Int) { ToggleFavourite(mangaId: $id) { manga { nodes { id } } } }`, map[string]any{"id": mediaID}, &resp)
}

// MediaEntry returns the authenticated user's list entry for a media.
func (c *AniListClient) MediaEntry(ctx context.Context, mediaID int) (progress int, status string, found bool, err error) {
	var resp struct {
		Data struct {
			Media struct {
				MediaListEntry *struct {
					Progress int    `json:"progress"`
					Status   string `json:"status"`
				} `json:"mediaListEntry"`
			} `json:"Media"`
		} `json:"data"`
	}
	if err := c.do(ctx, `
		query ($id: Int) { Media(id: $id, type: MANGA) { mediaListEntry { progress status } } }
	`, map[string]any{"id": mediaID}, &resp); err != nil {
		return 0, "", false, err
	}
	e := resp.Data.Media.MediaListEntry
	if e == nil {
		return 0, "", false, nil
	}
	return e.Progress, e.Status, true, nil
}

// SaveEntry upserts the authenticated user's list entry. An empty status
// keeps the remote status; progress < 0 keeps the remote progress.
func (c *AniListClient) SaveEntry(ctx context.Context, mediaID, progress int, status string) error {
	vars := map[string]any{"id": mediaID}
	if progress >= 0 {
		vars["progress"] = progress
	}
	if status != "" {
		vars["status"] = status
	}
	var resp struct {
		Data struct {
			SaveMediaListEntry struct {
				ID int `json:"id"`
			} `json:"SaveMediaListEntry"`
		} `json:"data"`
	}
	return c.do(ctx, `
		mutation ($id: Int, $progress: Int, $status: MediaListStatus) {
			SaveMediaListEntry(mediaId: $id, progress: $progress, status: $status) { id }
		}`, vars, &resp)
}

// DeleteEntry removes the authenticated user's list entry for a media entirely.
// The entry id is resolved via the viewer-scoped MediaList query rather than
// Media.mediaListEntry, which AniList serves from a CDN cache that can return
// a stale or foreign entry id. A missing entry is treated as already gone.
func (c *AniListClient) DeleteEntry(ctx context.Context, userID, mediaID int) error {
	var lookup struct {
		Data struct {
			MediaList *struct {
				ID int `json:"id"`
			} `json:"MediaList"`
		} `json:"data"`
	}
	if err := c.do(ctx, `
		query ($userId: Int, $mediaId: Int) { MediaList(userId: $userId, mediaId: $mediaId, type: MANGA) { id } }
	`, map[string]any{"userId": userID, "mediaId": mediaID}, &lookup); err != nil {
		if errors.Is(err, errAniListNotFound) {
			return nil
		}
		return err
	}
	if lookup.Data.MediaList == nil {
		return nil
	}
	var resp struct {
		Data struct {
			DeleteMediaListEntry struct {
				Deleted bool `json:"deleted"`
			} `json:"DeleteMediaListEntry"`
		} `json:"data"`
	}
	return c.do(ctx, `
		mutation ($id: Int) { DeleteMediaListEntry(id: $id) { deleted } }
	`, map[string]any{"id": lookup.Data.MediaList.ID}, &resp)
}
