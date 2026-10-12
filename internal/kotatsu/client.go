// Package kotatsu calls the kotatsu-worker sidecar for external manga sources.
package kotatsu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Source struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Language    string   `json:"language"`
	ContentType string   `json:"content_type"`
	NSFW        bool     `json:"nsfw"`
	Broken      bool     `json:"broken"`
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
	OK      bool   `json:"ok"`
	Parsers int    `json:"parsers"`
	Version string `json:"version"`
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

// Endpoint returns the worker base URL, for absolutizing /image paths.
func (c *Client) Endpoint() string {
	return c.endpoint
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

// Pages returns worker-relative, pre-signed /image paths in page order.
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
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kotatsu worker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
