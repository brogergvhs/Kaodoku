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
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/brogergvhs/kaodoku/internal/util"
)

type restClient struct {
	base        string
	label       string
	contentType string
	client      *http.Client
	limiter     *rate.Limiter
	auth        func(ctx context.Context, h http.Header)
	*health
}

func newRest(base, label string, client *http.Client, every time.Duration) restClient {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return restClient{base: base, label: label, contentType: "application/json", client: client, limiter: rate.NewLimiter(rate.Every(every), 3), health: &health{}}
}

func bearer(ctx context.Context, h http.Header) {
	if t := AccountFrom(ctx).Token; t != "" {
		h.Set("Authorization", "Bearer "+t)
	}
}

func (c *restClient) do(ctx context.Context, method, path string, body, out any) error {
	return c.observe(ctx, c.send(ctx, method, path, body, out))
}

func (c *restClient) send(ctx context.Context, method, path string, body, out any) error {
	target := path
	if !strings.HasPrefix(path, "http") {
		target = c.base + path
	}
	var payload []byte
	contentType := c.contentType
	switch b := body.(type) {
	case nil:
	case url.Values:
		payload, contentType = []byte(b.Encode()), "application/x-www-form-urlencoded"
	default:
		var err error
		if payload, err = json.Marshal(b); err != nil {
			return err
		}
	}
	for attempt := 1; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if payload != nil {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Accept", c.contentType)
		req.Header.Set("User-Agent", util.PickUserAgent(""))
		if c.auth != nil {
			c.auth(ctx, req.Header)
		}
		c.count(ctx)
		resp, err := c.client.Do(req)
		if err != nil {
			return transportError(c.label, err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt < 3, resp.StatusCode == http.StatusPreconditionFailed && bytes.Contains(data, []byte("Please wait")) && attempt < 3:
			wait := retryAfter(resp)
			if resp.StatusCode == http.StatusPreconditionFailed {
				wait = 2 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("%s HTTP 401: %w", c.label, ErrUnauthorized)
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%s HTTP 404: %w", c.label, ErrNotFound)
		case resp.StatusCode >= http.StatusBadGateway && resp.StatusCode <= http.StatusGatewayTimeout:
			return fmt.Errorf("%s HTTP %d %w: %s", c.label, resp.StatusCode, ErrUnavailable, outageReason(data))
		case resp.StatusCode >= http.StatusBadRequest:
			return fmt.Errorf("%s HTTP %d: %.200s", c.label, resp.StatusCode, data)
		}
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s response: %w", c.label, err)
		}
		return nil
	}
}

func retryAfter(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return time.Minute
	}
	return min(time.Duration(seconds)*time.Second, 90*time.Second)
}

type oauthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (c *restClient) token(ctx context.Context, tokenURL string, form url.Values) (Token, error) {
	var t oauthToken
	if err := c.do(ctx, http.MethodPost, tokenURL, form, &t); err != nil {
		return Token{}, err
	}
	if t.AccessToken == "" {
		return Token{}, fmt.Errorf("%s token exchange: unexpected response", c.label)
	}
	return Token{Access: t.AccessToken, Refresh: t.RefreshToken, ExpiresIn: t.ExpiresIn}, nil
}

func intPtr(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	*f = flexString(strings.Trim(string(b), `"`))
	if *f == "null" {
		*f = ""
	}
	return nil
}
