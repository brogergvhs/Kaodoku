package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutagesDisableUntilASuccess(t *testing.T) {
	status, body := http.StatusServiceUnavailable, `{"error":"maintenance","message":"MyAnimeList is temporarily in maintenance mode."}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()

	mal := NewMALClient(nil)
	mal.base = srv.URL
	mal.SetClientID("x")
	if _, err := mal.Trending(ctx, 1); !IsUnavailable(err) || !strings.Contains(mal.Outage(), "maintenance mode") {
		t.Fatalf("err=%v outage=%q", err, mal.Outage())
	}
	status, body = http.StatusOK, `{"data":[]}`
	if _, err := mal.Trending(ctx, 1); err != nil || mal.Outage() != "" {
		t.Fatalf("recovery: err=%v outage=%q", err, mal.Outage())
	}

	ani := NewAniListClient(nil)
	ani.endpoint = srv.URL
	status, body = http.StatusForbidden, `{"errors":[{"message":"The AniList API has been temporarily disabled due to severe stability issues.","status":403}],"data":null}`
	if _, err := ani.Trending(ctx, 1); !IsUnavailable(err) || !strings.Contains(ani.Outage(), "temporarily disabled") {
		t.Fatalf("err=%v outage=%q", err, ani.Outage())
	}
	status, body = http.StatusForbidden, `{"errors":[{"message":"Forbidden"}]}`
	ani = NewAniListClient(nil)
	ani.endpoint = srv.URL
	if _, err := ani.Trending(ctx, 1); err == nil || IsUnavailable(err) || ani.Outage() != "" {
		t.Fatalf("plain 403 is not an outage: %v", err)
	}
}
