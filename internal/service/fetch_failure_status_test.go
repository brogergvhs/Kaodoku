package service

import (
	"errors"
	"fmt"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/sources"
)

func TestFetchFailureStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"cloudflare worker error", &kotatsu.APIError{Code: "cloudflare_challenge", Message: "cloudflare challenge at https://x"}, sources.StatusRequiresCF},
		{"wrapped cloudflare", fmt.Errorf("fetch chapters: %w", &kotatsu.APIError{Code: "cloudflare_challenge"}), sources.StatusRequiresCF},
		{"rate limited", &kotatsu.APIError{Code: "rate_limited"}, sources.StatusDegraded},
		{"upstream http", &kotatsu.APIError{Code: "upstream_http"}, sources.StatusBroken},
		{"plain text mentioning cloudflare", errors.New("blocked by Cloudflare"), sources.StatusRequiresCF},
		{"other", errors.New("connection refused"), sources.StatusBroken},
	}
	for _, c := range cases {
		if got := fetchFailureStatus(c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
