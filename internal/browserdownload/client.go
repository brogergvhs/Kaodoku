// Package browserdownload calls an external browser worker for chapter downloads.
package browserdownload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultEndpoint = "http://localhost:8192"

type Request struct {
	ChapterURL        string   `json:"chapter_url"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

type Result struct {
	Images int
	Bytes  int64
}

type Client struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func New(endpoint string, timeout time.Duration, client *http.Client) *Client {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultEndpoint
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   strings.TrimSpace(os.Getenv("KAODOKU_BROWSER_DOWNLOADER_API_KEY")),
		client:   client,
	}
}

// Health reports whether the browser worker is reachable, and its container memory when it says (0 otherwise).
func (c *Client) Health(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/health", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("call browser downloader: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return 0, fmt.Errorf("browser downloader status %s", resp.Status)
	}
	var h struct {
		MemoryBytes int64 `json:"memory_bytes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h.MemoryBytes, nil
}

func (c *Client) DownloadCBZ(ctx context.Context, req Request, output string) (Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/download", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-Worker-Key", c.apiKey)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("call browser downloader: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Result{}, fmt.Errorf("browser downloader status %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return Result{}, err
	}
	tmp := output + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return Result{}, err
	}
	bytesWritten, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return Result{}, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return Result{}, closeErr
	}
	if err := os.Rename(tmp, output); err != nil {
		_ = os.Remove(tmp)
		return Result{}, err
	}

	images, _ := strconv.Atoi(resp.Header.Get("X-Kaodoku-Images"))
	return Result{Images: images, Bytes: bytesWritten}, nil
}
