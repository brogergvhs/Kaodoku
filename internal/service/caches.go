package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/util"
)

const (
	SettingCacheImagesMB  = "cache.images_mb"
	SettingCacheListsMB   = "cache.lists_mb"
	SettingCacheRelatedMB = "cache.related_mb"
	SettingCacheSearches  = "cache.search_entries"
	SettingCacheRecs      = "cache.recs_users"
)

// CacheKeys are the cache size limits, in MB or entries; 0 turns a cache off.
func CacheKeys() []string {
	return []string{SettingCacheImagesMB, SettingCacheListsMB, SettingCacheRelatedMB, SettingCacheSearches, SettingCacheRecs}
}

func (s *JobService) cacheLimit(ctx context.Context, key string) int {
	n, _ := strconv.Atoi(s.Setting(ctx, key, SettingDefault(key)))
	return n
}

var imageClient, _ = util.NewHTTPClient(util.HTTPClientOptions{Timeout: 20 * time.Second, BlockPrivateNetworks: true})

func (s *JobService) imageDir() string {
	return filepath.Join(filepath.Dir(s.dbPath), "cache", "images")
}

// ImageURL routes tracker-hosted images through the server's image cache; other URLs stay as they are.
func (s *JobService) ImageURL(raw string) string {
	if catalog.ImageProvider(raw) == "" {
		return raw
	}
	return "/img/" + s.secrets.Sign("img:"+raw) + "/" + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// SignedImage is the URL behind an /img path, or "" when the signature doesn't match.
func (s *JobService) SignedImage(sig, key string) string {
	raw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || !hmac.Equal([]byte(sig), []byte(s.secrets.Sign("img:"+string(raw)))) || catalog.ImageProvider(string(raw)) == "" {
		return ""
	}
	return string(raw)
}

// Image returns a remote image from the disk cache, fetching and keeping it on a miss.
func (s *JobService) Image(ctx context.Context, rawURL, reason string) ([]byte, error) {
	sum := sha256.Sum256([]byte(rawURL))
	path := filepath.Join(s.imageDir(), hex.EncodeToString(sum[:]))
	if data, err := os.ReadFile(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return data, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil || rawURL == "" {
		return nil, fmt.Errorf("invalid image url")
	}
	s.CountImage(rawURL, reason)
	resp, err := imageClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("empty image")
	}
	if limit := int64(s.cacheLimit(ctx, SettingCacheImagesMB)) << 20; limit > 0 && os.MkdirAll(s.imageDir(), 0o755) == nil && os.WriteFile(path, data, 0o644) == nil {
		s.trimImages(limit)
	}
	return data, nil
}

// trimImages deletes the least recently used images until the cache fits.
// ponytail: scans the folder on every miss; keep a running total if image counts grow large.
func (s *JobService) trimImages(limit int64) {
	entries, _ := os.ReadDir(s.imageDir())
	type file struct {
		path string
		size int64
		used time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			files, total = append(files, file{filepath.Join(s.imageDir(), e.Name()), info.Size(), info.ModTime()}), total+info.Size()
		}
	}
	slices.SortFunc(files, func(a, b file) int { return a.used.Compare(b.used) })
	for _, f := range files {
		if total <= limit {
			return
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// trimRows drops rows past the newest ones that fit limit bytes; query yields two key columns and a size, newest first.
func (s *JobService) trimRows(ctx context.Context, limit int64, query string, drop func(a, b any)) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return
	}
	var total int64
	var old [][2]any
	for rows.Next() {
		var a, b any
		var size int64
		if rows.Scan(&a, &b, &size) == nil {
			if total += size; total > limit {
				old = append(old, [2]any{a, b})
			}
		}
	}
	rows.Close()
	for _, k := range old {
		drop(k[0], k[1])
	}
}

func (s *JobService) trimLists(ctx context.Context) {
	s.trimRows(ctx, int64(s.cacheLimit(ctx, SettingCacheListsMB))<<20,
		`SELECT user_id, provider, LENGTH(list_json) FROM tracker_accounts WHERE list_json != '' ORDER BY list_fetched_at DESC`,
		func(uid, provider any) {
			_, _ = s.db.ExecContext(ctx, `UPDATE tracker_accounts SET list_json = '', list_fetched_at = '' WHERE user_id = ? AND provider = ?`, uid, provider)
			s.forgetList(uid.(int64), fmt.Sprint(provider))
		})
}

func (s *JobService) trimRelated(ctx context.Context) {
	s.trimRows(ctx, int64(s.cacheLimit(ctx, SettingCacheRelatedMB))<<20,
		`SELECT provider, provider_id, LENGTH(relations_json) + LENGTH(recommendations_json) FROM tracker_related ORDER BY MAX(relations_fetched_at, recommendations_fetched_at) DESC`,
		func(provider, id any) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM tracker_related WHERE provider = ? AND provider_id = ?`, provider, id)
		})
}

func trimOldest[K comparable, V any](m map[K]V, limit int, expires func(V) time.Time) {
	for len(m) > max(limit, 0) {
		var oldest K
		var at time.Time
		for k, v := range m {
			if t := expires(v); at.IsZero() || t.Before(at) {
				oldest, at = k, t
			}
		}
		delete(m, oldest)
	}
}

// CacheUsage is each cache's current size, in bytes or entries like its limit.
func (s *JobService) CacheUsage(ctx context.Context) map[string]int64 {
	var images, lists, related int64
	entries, _ := os.ReadDir(s.imageDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			images += info.Size()
		}
	}
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(list_json)), 0) FROM tracker_accounts`).Scan(&lists)
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(relations_json) + LENGTH(recommendations_json)), 0) FROM tracker_related`).Scan(&related)
	s.catalog.searchMu.Lock()
	searches := len(s.catalog.searches)
	s.catalog.searchMu.Unlock()
	s.recMu.Lock()
	recs := len(s.recCache)
	s.recMu.Unlock()
	return map[string]int64{SettingCacheImagesMB: images, SettingCacheListsMB: lists, SettingCacheRelatedMB: related, SettingCacheSearches: int64(searches), SettingCacheRecs: int64(recs)}
}

// ClearCache empties the cache named by its limit setting.
func (s *JobService) ClearCache(ctx context.Context, key string) error {
	switch key {
	case SettingCacheImagesMB:
		return os.RemoveAll(s.imageDir())
	case SettingCacheListsMB:
		s.listMu.Lock()
		s.listCache = nil
		s.listMu.Unlock()
		_, err := s.db.ExecContext(ctx, `UPDATE tracker_accounts SET list_json = '', list_fetched_at = ''`)
		return err
	case SettingCacheRelatedMB:
		_, err := s.db.ExecContext(ctx, `DELETE FROM tracker_related`)
		return err
	case SettingCacheSearches:
		s.catalog.searchMu.Lock()
		s.catalog.searches = nil
		s.catalog.searchMu.Unlock()
	case SettingCacheRecs:
		s.recMu.Lock()
		s.recCache, s.sugCache = nil, nil
		s.recMu.Unlock()
	default:
		return fmt.Errorf("unknown cache %q", key)
	}
	return nil
}
