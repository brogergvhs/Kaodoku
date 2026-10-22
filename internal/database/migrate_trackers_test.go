package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateMovesAniListAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO roles (id, name) VALUES (9, 'readers')`,
		`INSERT INTO users (id, username, role_id) VALUES (5, 'reader', 9)`,
		`CREATE TABLE user_anilist (user_id INTEGER PRIMARY KEY, access_token TEXT NOT NULL, anilist_user_id INTEGER NOT NULL DEFAULT 0, anilist_name TEXT NOT NULL DEFAULT '', expires_at TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO user_anilist (user_id, access_token, anilist_user_id, anilist_name, expires_at) VALUES (5, 'enc:x', 42, 'ann', '2027-01-01 00:00:00')`,
		`INSERT INTO jobs (type, status, payload_json, run_after) VALUES ('sync_anilist', 'queued', '{}', CURRENT_TIMESTAMP)`,
		`INSERT INTO titles (id, source_url, display_title) VALUES (7, 'seed://7', 't')`,
		`INSERT INTO chapters (id, title_id, label, url) VALUES (70, 7, '1', 'seed://7/1')`,
		`INSERT INTO chapter_read_progress (user_id, chapter_id, completed) VALUES (5, 70, 1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var provider, token, remote, name, expires string
	if err := db.QueryRowContext(ctx, `SELECT provider, access_token, remote_user_id, remote_name, expires_at FROM tracker_accounts WHERE user_id = 5`).Scan(&provider, &token, &remote, &name, &expires); err != nil {
		t.Fatal(err)
	}
	if provider != "anilist" || token != "enc:x" || remote != "42" || name != "ann" || expires != "2027-01-01 00:00:00" {
		t.Fatalf("migrated account = %s %s %s %s %s", provider, token, remote, name, expires)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'user_anilist'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user_anilist still present (%d, %v)", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE type = 'sync_trackers'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("queued sync job not renamed (%d, %v)", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tracker_title_sync WHERE user_id = 5 AND title_id = 7 AND provider = 'anilist'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read title did not keep AniList sync (%d, %v)", n, err)
	}
}

func TestMigrateRetriesTitleMatchMissesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	seed := func() {
		for _, q := range []string{
			`INSERT OR IGNORE INTO catalog_manga (id, provider, provider_id) VALUES (1, 'anilist', '115240')`,
			`INSERT OR REPLACE INTO catalog_external_ids (catalog_manga_id, provider, method) VALUES (1, 'mangaupdates', 'miss')`,
			`INSERT OR REPLACE INTO catalog_external_ids (catalog_manga_id, provider, provider_id, method) VALUES (1, 'kitsu', '9', 'title_match')`,
		} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	methods := func() (out []string) {
		rows, _ := db.QueryContext(ctx, `SELECT method FROM catalog_external_ids ORDER BY provider`)
		defer rows.Close()
		for rows.Next() {
			var m string
			_ = rows.Scan(&m)
			out = append(out, m)
		}
		return out
	}
	seed()
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := methods(); len(got) != 1 || got[0] != "title_match" {
		t.Fatalf("after upgrade: %v, want only the suggestion kept", got)
	}
	seed()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := methods(); len(got) != 2 {
		t.Fatalf("misses cleared again on a later start: %v", got)
	}
}
