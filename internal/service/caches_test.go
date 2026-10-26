package service

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageCacheSignsServesAndTrims(t *testing.T) {
	svc, ctx := openTestJobs(t)
	raw := "https://s4.anilist.co/file/cover.jpg"
	u := svc.ImageURL(raw)
	parts := strings.Split(u, "/")
	if len(parts) != 4 || svc.SignedImage(parts[2], parts[3]) != raw || svc.SignedImage("forged", parts[3]) != "" {
		t.Fatalf("signed url %q does not round trip", u)
	}
	if other := "https://example.test/c.jpg"; svc.ImageURL(other) != other {
		t.Fatal("only tracker images go through the cache")
	}
	dir := svc.imageDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := svc.Image(ctx, raw, "Test"); err != nil || string(data) != "cached" {
		t.Fatalf("cache hit = %q, %v", data, err)
	}
	for i, name := range []string{"old", "mid", "new"} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, make([]byte, 100), 0o644)
		at := time.Now().Add(time.Duration(i-10) * time.Hour)
		_ = os.Chtimes(path, at, at)
	}
	svc.trimImages(250)
	if _, err := os.Stat(filepath.Join(dir, "old")); !os.IsNotExist(err) {
		t.Fatal("the least recently used image should go first")
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); err != nil {
		t.Fatal("recent images should stay")
	}

	for i, id := range []string{"1", "2"} {
		if _, err := svc.db.ExecContext(ctx, `INSERT INTO tracker_related (provider, provider_id, relations_json, recommendations_json, relations_fetched_at) VALUES ('anilist', ?, ?, '[]', ?)`, id, strings.Repeat("x", 600_000), time.Now().Add(time.Duration(i)*time.Hour).Format(time.DateTime)); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.SetSetting(ctx, SettingCacheRelatedMB, "1"); err != nil {
		t.Fatal(err)
	}
	svc.trimRelated(ctx)
	var left string
	if err := svc.db.QueryRowContext(ctx, `SELECT GROUP_CONCAT(provider_id) FROM tracker_related`).Scan(&left); err != nil || left != "2" {
		t.Fatalf("related rows left = %q, %v; want only the newest", left, err)
	}
}
