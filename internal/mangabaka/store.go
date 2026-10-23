// Package mangabaka keeps a compact local copy of the MangaBaka series database.
package mangabaka

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const DumpBase = "https://api.mangabaka.org/v1/database/"

// Trackers are the tracker names the dump carries ids for, as Kaodoku names them.
var Trackers = []string{"anilist", "mal", "kitsu", "mangaupdates"}

// DumpURL is the JSON Lines dump for a variant; "full" adds MangaUpdates recommendations.
func DumpURL(variant string) string {
	if variant == "full" {
		return DumpBase + "series.full.jsonl.tar.gz"
	}
	return DumpBase + "series.jsonl.tar.gz"
}

type Store struct {
	path string
	mu   sync.RWMutex
	db   *sql.DB
}

// Open uses the copy at path when there is one.
func Open(path string) *Store {
	s := &Store{path: path}
	if _, err := os.Stat(path); err == nil {
		s.db, _ = openReadOnly(path)
	}
	return s
}

func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (s *Store) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db != nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

type Meta struct {
	SHA1, Variant string
	ImportedAt    time.Time
	Series        int
	Size          int64
}

func (s *Store) Meta(ctx context.Context) Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var m Meta
	if s.db == nil {
		return m
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) != nil {
			continue
		}
		switch k {
		case "sha1":
			m.SHA1 = v
		case "variant":
			m.Variant = v
		case "imported_at":
			m.ImportedAt, _ = time.Parse(time.RFC3339, v)
		case "series":
			m.Series, _ = strconv.Atoi(v)
		}
	}
	if info, err := os.Stat(s.path); err == nil {
		m.Size = info.Size()
	}
	return m
}

// RemoteSHA1 is the published checksum of the dump at url.
func RemoteSHA1(ctx context.Context, client *http.Client, url string) (string, error) {
	body, err := get(ctx, client, url+".sha1")
	if err != nil {
		return "", err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 1024))
	if f := strings.Fields(string(data)); err == nil && len(f) > 0 {
		return f[0], nil
	}
	return "", fmt.Errorf("mangabaka checksum: unreadable")
}

func get(ctx context.Context, client *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("mangabaka %s: HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// Refresh imports the dump at url unless its checksum matches the current copy; force imports it anyway.
func (s *Store) Refresh(ctx context.Context, client *http.Client, url, variant string, force bool, progress func(int)) (bool, error) {
	sum, err := RemoteSHA1(ctx, client, url)
	if err != nil {
		return false, err
	}
	if m := s.Meta(ctx); !force && sum == m.SHA1 && m.Variant == variant {
		return false, nil
	}
	tmp := s.path + ".tmp"
	_ = os.Remove(tmp)
	if err := importDump(ctx, client, url, tmp, sum, variant, progress); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(s.path); statErr == nil {
			s.db, _ = openReadOnly(s.path)
		}
		return false, err
	}
	s.db, err = openReadOnly(s.path)
	return true, err
}

const schema = `
CREATE TABLE series (
	id INTEGER PRIMARY KEY, merged_into INTEGER,
	anilist INTEGER, mal INTEGER, kitsu INTEGER, mangaupdates INTEGER,
	type TEXT, status TEXT, content_rating TEXT, year INTEGER, chapters INTEGER, volumes INTEGER,
	title TEXT, english TEXT, native TEXT, romanized TEXT, titles TEXT, description TEXT, cover TEXT,
	genres TEXT, tags TEXT, authors TEXT, rating REAL, popularity INTEGER, popularity_week INTEGER,
	start_date TEXT, end_date TEXT, updated_at TEXT
);
CREATE TABLE relations (series_id INTEGER, to_series_id INTEGER, type TEXT);
CREATE TABLE mu_recs (series_id INTEGER, mu INTEGER, weight INTEGER);
CREATE TABLE recommendations (series_id INTEGER, rec_id INTEGER, weight INTEGER);
CREATE TABLE ambiguous (provider TEXT, provider_id INTEGER, PRIMARY KEY (provider, provider_id));
CREATE VIRTUAL TABLE titles USING fts5(title, series_id UNINDEXED, tokenize = 'unicode61 remove_diacritics 2');
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);`

const finish = `
CREATE INDEX series_anilist ON series(anilist) WHERE anilist IS NOT NULL;
CREATE INDEX series_mal ON series(mal) WHERE mal IS NOT NULL;
CREATE INDEX series_kitsu ON series(kitsu) WHERE kitsu IS NOT NULL;
CREATE INDEX series_mangaupdates ON series(mangaupdates) WHERE mangaupdates IS NOT NULL;
CREATE INDEX series_popularity ON series(popularity) WHERE merged_into IS NULL AND popularity IS NOT NULL;
CREATE INDEX relations_series ON relations(series_id);
INSERT INTO recommendations SELECT r.series_id, s.id, r.weight FROM mu_recs r JOIN series s ON s.mangaupdates = r.mu AND s.id != r.series_id;
DROP TABLE mu_recs;
CREATE INDEX recommendations_series ON recommendations(series_id);
INSERT INTO ambiguous SELECT 'anilist', anilist FROM series WHERE anilist IS NOT NULL GROUP BY anilist HAVING COUNT(*) > 1;
INSERT INTO ambiguous SELECT 'mal', mal FROM series WHERE mal IS NOT NULL GROUP BY mal HAVING COUNT(*) > 1;
INSERT INTO ambiguous SELECT 'kitsu', kitsu FROM series WHERE kitsu IS NOT NULL GROUP BY kitsu HAVING COUNT(*) > 1;
INSERT INTO ambiguous SELECT 'mangaupdates', mangaupdates FROM series WHERE mangaupdates IS NOT NULL GROUP BY mangaupdates HAVING COUNT(*) > 1;`

type record struct {
	ID              int64  `json:"id"`
	State           string `json:"state"`
	MergedWith      int64  `json:"merged_with"`
	Title           string `json:"title"`
	NativeTitle     string `json:"native_title"`
	RomanizedTitle  string `json:"romanized_title"`
	SecondaryTitles map[string][]struct {
		Title string `json:"title"`
	} `json:"secondary_titles"`
	Titles []struct {
		Title     string `json:"title"`
		Language  string `json:"language"`
		IsPrimary bool   `json:"is_primary"`
	} `json:"titles"`
	Description   string   `json:"description"`
	Type          string   `json:"type"`
	Status        string   `json:"status"`
	ContentRating string   `json:"content_rating"`
	Year          int      `json:"year"`
	TotalChapters string   `json:"total_chapters"`
	FinalVolume   string   `json:"final_volume"`
	Authors       []string `json:"authors"`
	Genres        []string `json:"genres"`
	Tags          []string `json:"tags"`
	Rating        float64  `json:"rating"`
	LastUpdatedAt string   `json:"last_updated_at"`
	Cover         struct {
		Raw struct {
			URL string `json:"url"`
		} `json:"raw"`
	} `json:"cover"`
	Published struct {
		StartDate string `json:"start_date"`
		EndDate   string `json:"end_date"`
	} `json:"published"`
	Popularity struct {
		Global struct {
			Current float64 `json:"current"`
			History struct {
				Week int64 `json:"1w"`
			} `json:"history"`
		} `json:"global"`
	} `json:"popularity"`
	Relationships []struct {
		To   int64  `json:"to_series_id"`
		Type string `json:"relation_type"`
	} `json:"relationships_v2"`
	Source struct {
		AniList      struct{ ID int64 } `json:"anilist"`
		MAL          struct{ ID int64 } `json:"my_anime_list"`
		Kitsu        struct{ ID int64 } `json:"kitsu"`
		MangaUpdates struct {
			ID       string `json:"id"`
			Response struct {
				Recommendations []struct {
					SeriesID int64 `json:"series_id"`
					Weight   int64 `json:"weight"`
				} `json:"recommendations"`
			} `json:"response"`
		} `json:"manga_updates"`
	} `json:"source"`
}

func importDump(ctx context.Context, client *http.Client, url, path, sum, variant string, progress func(int)) error {
	resp, err := get(ctx, client, url)
	if err != nil {
		return err
	}
	defer resp.Close()
	hash := sha1.New()
	body := io.TeeReader(resp, hash)
	gz, err := gzip.NewReader(body)
	if err != nil {
		return fmt.Errorf("mangabaka dump: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("mangabaka dump has no series file: %w", err)
		}
		if h.Typeflag == tar.TypeReg && strings.HasSuffix(h.Name, ".jsonl") {
			break
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := prepare(ctx, tx)
	if err != nil {
		return err
	}
	n := 0
	for dec := json.NewDecoder(tr); dec.More(); {
		var r record
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("mangabaka dump line %d: %w", n+1, err)
		}
		if err := ins.add(ctx, r); err != nil {
			return err
		}
		if n++; n%10000 == 0 {
			if progress != nil {
				progress(n)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	_, _ = io.Copy(io.Discard, gz)
	_, _ = io.Copy(io.Discard, body)
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum {
		return fmt.Errorf("mangabaka dump checksum mismatch (got %s, want %s)", got, sum)
	}
	if _, err := tx.ExecContext(ctx, finish); err != nil {
		return err
	}
	for k, v := range map[string]string{"sha1": sum, "variant": variant, "imported_at": time.Now().UTC().Format(time.RFC3339), "series": strconv.Itoa(ins.active)} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type inserter struct {
	series, merged, relation, muRec, title *sql.Stmt
	active                                 int
}

func prepare(ctx context.Context, tx *sql.Tx) (*inserter, error) {
	var in inserter
	var errs []error
	p := func(q string) *sql.Stmt {
		st, err := tx.PrepareContext(ctx, q)
		errs = append(errs, err)
		return st
	}
	in.series = p(`INSERT INTO series VALUES (?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	in.merged = p(`INSERT INTO series (id, merged_into) VALUES (?, ?)`)
	in.relation = p(`INSERT INTO relations VALUES (?, ?, ?)`)
	in.muRec = p(`INSERT INTO mu_recs VALUES (?, ?, ?)`)
	in.title = p(`INSERT INTO titles VALUES (?, ?)`)
	return &in, errors.Join(errs...)
}

func (in *inserter) add(ctx context.Context, r record) error {
	switch r.State {
	case "merged":
		if r.MergedWith > 0 {
			_, err := in.merged.ExecContext(ctx, r.ID, r.MergedWith)
			return err
		}
		return nil
	case "active":
	default:
		return nil
	}
	in.active++
	english := ""
	titles := []string{}
	seen := map[string]bool{}
	addTitle := func(t string) {
		if t = strings.TrimSpace(t); t != "" && !seen[strings.ToLower(t)] {
			seen[strings.ToLower(t)] = true
			titles = append(titles, t)
		}
	}
	for _, t := range []string{r.Title, r.RomanizedTitle, r.NativeTitle} {
		addTitle(t)
	}
	for _, t := range r.Titles {
		if english == "" && t.Language == "en" && t.IsPrimary {
			english = t.Title
		}
		addTitle(t.Title)
	}
	for _, list := range r.SecondaryTitles {
		for _, t := range list {
			addTitle(t.Title)
		}
	}
	mu, _ := strconv.ParseInt(r.Source.MangaUpdates.ID, 36, 64)
	if _, err := in.series.ExecContext(ctx, r.ID,
		orNull(r.Source.AniList.ID), orNull(r.Source.MAL.ID), orNull(r.Source.Kitsu.ID), orNull(mu),
		r.Type, r.Status, r.ContentRating, orNull(int64(r.Year)), orNull(count(r.TotalChapters)), orNull(count(r.FinalVolume)),
		r.Title, english, r.NativeTitle, r.RomanizedTitle, jsonList(titles), r.Description, r.Cover.Raw.URL,
		jsonList(r.Genres), jsonList(r.Tags), jsonList(r.Authors), r.Rating,
		orNull(int64(r.Popularity.Global.Current)), orNull(r.Popularity.Global.History.Week),
		r.Published.StartDate, r.Published.EndDate, r.LastUpdatedAt); err != nil {
		return err
	}
	for _, t := range titles {
		if _, err := in.title.ExecContext(ctx, t, r.ID); err != nil {
			return err
		}
	}
	for _, rel := range r.Relationships {
		if _, err := in.relation.ExecContext(ctx, r.ID, rel.To, rel.Type); err != nil {
			return err
		}
	}
	for _, rec := range r.Source.MangaUpdates.Response.Recommendations {
		if _, err := in.muRec.ExecContext(ctx, r.ID, rec.SeriesID, rec.Weight); err != nil {
			return err
		}
	}
	return nil
}

func orNull(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func count(s string) int64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return int64(f)
}

func jsonList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	data, _ := json.Marshal(v)
	return string(data)
}
