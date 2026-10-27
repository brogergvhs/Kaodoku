package kotatsu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type APIError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
	Source  string `json:"source"`
}

func (e *APIError) Error() string {
	s := "kotatsu " + e.Code + ": " + e.Message
	if e.Source != "" {
		s += " (" + e.Source + ")"
	}
	return s
}

func (e *APIError) IsCloudflare() bool { return e.Code == "cloudflare_challenge" }

type Source struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Language    string   `json:"language"`
	ContentType string   `json:"content_type"`
	NSFW        bool     `json:"nsfw"`
	Broken      bool     `json:"broken"`
	Cloudflare  bool     `json:"cloudflare"`
	Searchable  bool     `json:"searchable"`
	Domains     []string `json:"domains"`
	BaseURL     string   `json:"base_url"`
}

type SearchResult struct {
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	Cover     string   `json:"cover"`
	AltTitles []string `json:"alt_titles"`
}

type Chapter struct {
	ID         int64  `json:"id"`
	URL        string `json:"url"`
	Number     string `json:"number"`
	Title      string `json:"title"`
	Volume     int    `json:"volume"`
	Branch     string `json:"branch"`
	UploadedAt string `json:"uploaded_at"`
}

type Chapters struct {
	ResolvedURL string    `json:"resolved_url"`
	Chapters    []Chapter `json:"chapters"`
}

type Health struct {
	OK          bool   `json:"ok"`
	Version     string `json:"version"`
	Solver      bool   `json:"solver"`
	MemoryBytes int64  `json:"memory_bytes"`
}

type Client struct {
	endpoint string
	key      string
	client   *http.Client
}

func New(endpoint, key string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		key:      key,
		client:   &http.Client{Timeout: 180 * time.Second},
	}
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	return out, c.get(ctx, "/healthz", nil, &out)
}

func (c *Client) Sources(ctx context.Context) ([]Source, error) {
	var out []Source
	return out, c.get(ctx, "/sources", nil, &out)
}

func (c *Client) Search(ctx context.Context, source, query string) ([]SearchResult, error) {
	var out []SearchResult
	return out, c.get(ctx, "/search", url.Values{"source": {source}, "q": {query}}, &out)
}

func (c *Client) Chapters(ctx context.Context, source, mangaURL string) (Chapters, error) {
	var out Chapters
	return out, c.get(ctx, "/chapters", url.Values{"source": {source}, "url": {mangaURL}}, &out)
}

func (c *Client) Pages(ctx context.Context, source, mangaURL, chapterURL string) ([]string, error) {
	var raw []struct {
		Image string `json:"image"`
	}
	err := c.get(ctx, "/pages", url.Values{
		"source": {source}, "manga": {mangaURL}, "chapter": {chapterURL},
	}, &raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(raw))
	for i, p := range raw {
		out[i] = c.endpoint + p.Image
	}
	return out, nil
}

func (c *Client) SetConfig(ctx context.Context, source string, values map[string]string) error {
	body, err := json.Marshal(map[string]any{"source": source, "values": values})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Worker-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("kotatsu worker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return apiError("/config", resp)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.endpoint + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Worker-Key", c.key)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("kotatsu worker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(path, resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiError(path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var e APIError
	if json.Unmarshal(body, &e) == nil && e.Code != "" {
		return &e
	}
	return fmt.Errorf("kotatsu worker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
}
