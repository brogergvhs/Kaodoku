package database

import (
	"context"
	"database/sql"
	"fmt"
)

const initialSchemaVersion = 1

const sourcesSchema = `
CREATE TABLE IF NOT EXISTS sources (
	id TEXT PRIMARY KEY,
	origin TEXT NOT NULL DEFAULT 'local',
	name TEXT NOT NULL,
	domains_json TEXT NOT NULL DEFAULT '[]',
	base_url TEXT NOT NULL DEFAULT '',
	sample_manga_url TEXT NOT NULL DEFAULT '',
	search_url TEXT NOT NULL DEFAULT '',
	scraper TEXT NOT NULL DEFAULT 'generic',
	allowed_extensions_json TEXT NOT NULL DEFAULT '[]',
	min_chapters INTEGER NOT NULL DEFAULT 0,
	requires_browser_solver INTEGER NOT NULL DEFAULT 0,
	requires_browser_downloader INTEGER NOT NULL DEFAULT 0,
	single_manga INTEGER NOT NULL DEFAULT 0,
	enabled INTEGER NOT NULL DEFAULT 1,
	profile_version TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'unknown',
	last_checked_at TEXT,
	last_error TEXT NOT NULL DEFAULT '',
	chapters_found INTEGER NOT NULL DEFAULT 0,
	sample_images_found INTEGER NOT NULL DEFAULT 0,
	image_extensions_json TEXT NOT NULL DEFAULT '[]',
	verify_steps_json TEXT NOT NULL DEFAULT '[]',
	chapter_fetch TEXT NOT NULL DEFAULT '',
	image_fetch TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

const initialSchema = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS volumes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	number REAL NOT NULL DEFAULT 0,
	name TEXT NOT NULL DEFAULT '',
	file TEXT NOT NULL,
	bytes INTEGER NOT NULL DEFAULT 0,
	pages INTEGER NOT NULL DEFAULT 0,
	cover BLOB,
	cover_type TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(title_id, file)
);
CREATE INDEX IF NOT EXISTS idx_volumes_title ON volumes(title_id);

CREATE TABLE IF NOT EXISTS volume_read_progress (
	user_id INTEGER NOT NULL DEFAULT 1,
	volume_id INTEGER NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
	completed INTEGER NOT NULL DEFAULT 0,
	last_read_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, volume_id)
);

CREATE TABLE IF NOT EXISTS library_screens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL DEFAULT 1,
	name TEXT NOT NULL,
	config_json TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_library_screens_user ON library_screens(user_id);

CREATE TABLE IF NOT EXISTS user_favourites (
	user_id INTEGER NOT NULL DEFAULT 1,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, title_id)
);

CREATE TABLE IF NOT EXISTS content_tags (
	name TEXT PRIMARY KEY,
	kind TEXT NOT NULL DEFAULT 'tag'
);
CREATE TABLE IF NOT EXISTS provider_tags (
	provider TEXT NOT NULL,
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'tag',
	category TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	slug TEXT NOT NULL DEFAULT '',
	is_adult INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (provider, name)
);
CREATE TABLE IF NOT EXISTS catalog_relations (
	from_id TEXT NOT NULL,
	to_id TEXT NOT NULL,
	relation TEXT NOT NULL,
	PRIMARY KEY (from_id, to_id)
);
CREATE INDEX IF NOT EXISTS idx_catalog_relations_from ON catalog_relations(from_id);

CREATE TABLE IF NOT EXISTS catalog_manga (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	provider TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	title_romaji TEXT NOT NULL DEFAULT '',
	title_english TEXT NOT NULL DEFAULT '',
	title_native TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	cover_image TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	format TEXT NOT NULL DEFAULT '',
	chapters INTEGER,
	volumes INTEGER,
	synonyms_json TEXT NOT NULL DEFAULT '[]',
	genres_json TEXT NOT NULL DEFAULT '[]',
	authors_json TEXT NOT NULL DEFAULT '[]',
	year INTEGER NOT NULL DEFAULT 0,
	average_score INTEGER NOT NULL DEFAULT 0,
	is_adult INTEGER NOT NULL DEFAULT 0,
	tags_json TEXT NOT NULL DEFAULT '[]',
	wanted INTEGER NOT NULL DEFAULT 0,
	raw_json TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(provider, provider_id)
);
` + sourcesSchema + `
CREATE TABLE IF NOT EXISTS titles (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	catalog_manga_id INTEGER REFERENCES catalog_manga(id) ON DELETE SET NULL,
	source_id TEXT REFERENCES sources(id) ON DELETE SET NULL,
	source_url TEXT NOT NULL UNIQUE,
	display_title TEXT NOT NULL,
	output_path TEXT NOT NULL DEFAULT '',
	monitored INTEGER NOT NULL DEFAULT 1,
	refresh_interval TEXT NOT NULL DEFAULT '',
	last_refreshed_at TEXT,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS collections (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_collections_user ON collections(user_id);

CREATE TABLE IF NOT EXISTS collection_members (
	collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	PRIMARY KEY (collection_id, title_id)
);

CREATE TABLE IF NOT EXISTS collection_smart_pins (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	smart_key TEXT NOT NULL,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	PRIMARY KEY (user_id, smart_key, title_id)
);

CREATE TABLE IF NOT EXISTS title_source_matches (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	catalog_manga_id INTEGER NOT NULL REFERENCES catalog_manga(id) ON DELETE CASCADE,
	source_id TEXT REFERENCES sources(id) ON DELETE SET NULL,
	source_url TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	confidence REAL NOT NULL DEFAULT 0,
	match_method TEXT NOT NULL DEFAULT 'manual',
	chapters_found INTEGER NOT NULL DEFAULT 0,
	sample_images_found INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	verified_at TEXT,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(catalog_manga_id, source_url)
);

CREATE TABLE IF NOT EXISTS chapters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	label TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	scraped_title TEXT NOT NULL DEFAULT '',
	title_custom INTEGER NOT NULL DEFAULT 0,
	volume_num REAL NOT NULL DEFAULT 0,
	url TEXT NOT NULL,
	number_main INTEGER NOT NULL DEFAULT 0,
	suffix_type TEXT NOT NULL DEFAULT '',
	suffix_num INTEGER NOT NULL DEFAULT 0,
	discovered_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(title_id, label),
	UNIQUE(title_id, url)
);

CREATE TABLE IF NOT EXISTS downloads (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	chapter_id INTEGER NOT NULL REFERENCES chapters(id) ON DELETE CASCADE,
	status TEXT NOT NULL,
	output_file TEXT NOT NULL DEFAULT '',
	bytes INTEGER NOT NULL DEFAULT 0,
	pages INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	started_at TEXT,
	completed_at TEXT,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(chapter_id)
);

CREATE TABLE IF NOT EXISTS chapter_read_progress (
	user_id INTEGER NOT NULL DEFAULT 1,
	chapter_id INTEGER NOT NULL REFERENCES chapters(id) ON DELETE CASCADE,
	last_page INTEGER NOT NULL DEFAULT 0,
	read_pages INTEGER NOT NULL DEFAULT 0,
	total_pages INTEGER NOT NULL DEFAULT 0,
	completed INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	last_read_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	completed_at TEXT,
	PRIMARY KEY (user_id, chapter_id)
);

CREATE TABLE IF NOT EXISTS chapter_read_pages (
	user_id INTEGER NOT NULL DEFAULT 1,
	chapter_id INTEGER NOT NULL REFERENCES chapters(id) ON DELETE CASCADE,
	page INTEGER NOT NULL,
	read_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(user_id, chapter_id, page)
);

CREATE TABLE IF NOT EXISTS title_sources (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	source_id TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(title_id, url)
);

CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	type TEXT NOT NULL,
	status TEXT NOT NULL,
	payload_json TEXT NOT NULL DEFAULT '{}',
	run_after TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	parent_id INTEGER,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
UPDATE OR IGNORE settings SET key = 'sync.trackers_every' WHERE key = 'sync.anilist_every';

CREATE TABLE IF NOT EXISTS roles (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	origin TEXT NOT NULL DEFAULT 'local',
	permissions_json TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL DEFAULT '',
	role_id INTEGER NOT NULL REFERENCES roles(id),
	origin TEXT NOT NULL DEFAULT 'local',
	allow_adult INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS user_settings (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (user_id, key)
);

CREATE TABLE IF NOT EXISTS tracker_accounts (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	access_token TEXT NOT NULL,
	refresh_token TEXT NOT NULL DEFAULT '',
	remote_user_id TEXT NOT NULL DEFAULT '',
	remote_name TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, provider)
);

CREATE TABLE IF NOT EXISTS tracker_title_sync (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	PRIMARY KEY (user_id, title_id, provider)
);

DROP TABLE IF EXISTS tracker_requests;

CREATE TABLE IF NOT EXISTS tracker_calls (
	provider TEXT NOT NULL,
	day TEXT NOT NULL,
	kind TEXT NOT NULL,
	reason TEXT NOT NULL,
	count INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (provider, day, kind, reason)
);

CREATE TABLE IF NOT EXISTS tracker_related (
	provider TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	relations_json TEXT NOT NULL,
	recommendations_json TEXT NOT NULL,
	relations_fetched_at TEXT NOT NULL DEFAULT '',
	recommendations_fetched_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (provider, provider_id)
);

CREATE TABLE IF NOT EXISTS user_ratings (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	rating INTEGER NOT NULL,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, title_id)
);

CREATE TABLE IF NOT EXISTS tracker_ratings (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	rating INTEGER NOT NULL,
	PRIMARY KEY (user_id, title_id, provider)
);

DROP TABLE IF EXISTS tracker_pushed;

CREATE TABLE IF NOT EXISTS tracker_synced (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	progress TEXT NOT NULL DEFAULT '',
	rating TEXT NOT NULL DEFAULT '',
	favourite TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (user_id, title_id, provider)
);

DROP TABLE IF EXISTS tracker_favourites;

CREATE TABLE IF NOT EXISTS catalog_external_ids (
	catalog_manga_id INTEGER NOT NULL REFERENCES catalog_manga(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	provider_id TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT '',
	confirmed INTEGER NOT NULL DEFAULT 0,
	checked_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (catalog_manga_id, provider)
);
CREATE INDEX IF NOT EXISTS idx_catalog_external_lookup ON catalog_external_ids(provider, provider_id) WHERE confirmed = 1;

CREATE TABLE IF NOT EXISTS mangabaka_suggestions (
	catalog_manga_id INTEGER NOT NULL REFERENCES catalog_manga(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	dismissed INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (catalog_manga_id, provider)
);

CREATE TABLE IF NOT EXISTS api_tokens (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at TEXT NOT NULL DEFAULT ''
);

DROP TABLE IF EXISTS notifications;

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);

CREATE TABLE IF NOT EXISTS browser_cookies (
	domain TEXT NOT NULL,
	path TEXT NOT NULL DEFAULT '/',
	name TEXT NOT NULL,
	value TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	secure INTEGER NOT NULL DEFAULT 0,
	http_only INTEGER NOT NULL DEFAULT 0,
	user_agent TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY(domain, path, name)
);

CREATE INDEX IF NOT EXISTS idx_titles_monitored ON titles(monitored);
CREATE INDEX IF NOT EXISTS idx_chapters_title_id ON chapters(title_id);
CREATE INDEX IF NOT EXISTS idx_downloads_chapter_id ON downloads(chapter_id);
CREATE INDEX IF NOT EXISTS idx_chapter_read_pages_chapter_id ON chapter_read_pages(chapter_id);
CREATE INDEX IF NOT EXISTS idx_volume_read_progress_volume ON volume_read_progress(volume_id);
CREATE INDEX IF NOT EXISTS idx_jobs_status_run_after ON jobs(status, run_after);
CREATE INDEX IF NOT EXISTS idx_browser_cookies_expires_at ON browser_cookies(expires_at);

INSERT OR IGNORE INTO schema_migrations(version) VALUES (1);
`

// Migrate applies all built-in database migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, initialSchema); err != nil {
		return fmt.Errorf("apply migration %d: %w", initialSchemaVersion, err)
	}
	if err = ensureColumn(ctx, tx, "sources", "requires_browser_downloader", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate sources.requires_browser_downloader: %w", err)
	}
	if err = ensureColumn(ctx, tx, "sources", "search_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate sources.search_url: %w", err)
	}
	if err = ensureColumn(ctx, tx, "sources", "languages_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err = ensureColumn(ctx, tx, "sources", "nsfw", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err = ensureColumn(ctx, tx, "sources", "chapterless", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err = ensureColumn(ctx, tx, "sources", "single_manga", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate sources.single_manga: %w", err)
	}
	if err = ensureColumn(ctx, tx, "sources", "verify_steps_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return fmt.Errorf("migrate sources.verify_steps_json: %w", err)
	}
	for _, col := range []string{"chapter_fetch", "image_fetch"} {
		if err = ensureColumn(ctx, tx, "sources", col, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("migrate sources.%s: %w", col, err)
		}
	}

	if err = ensureColumn(ctx, tx, "chapters", "scraped_title", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate chapters.scraped_title: %w", err)
	}
	if err = ensureColumn(ctx, tx, "chapters", "title_custom", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate chapters.title_custom: %w", err)
	}
	if err = ensureColumn(ctx, tx, "chapters", "volume_num", "REAL NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate chapters.volume_num: %w", err)
	}
	if err = ensureColumn(ctx, tx, "chapters", "volume_custom", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate chapters.volume_custom: %w", err)
	}
	if err = ensureColumn(ctx, tx, "chapters", "source_uid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate chapters.source_uid: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE chapters SET scraped_title = title
		WHERE scraped_title = '' AND title != '' AND title_custom = 0`); err != nil {
		return fmt.Errorf("backfill chapters.scraped_title: %w", err)
	}

	if err = migrateReadTablesPerUser(ctx, tx); err != nil {
		return err
	}
	hadManual, _, err := txHasColumn(ctx, tx, "chapter_read_progress", "manual")
	if err != nil {
		return err
	}
	hadFetchError, _, err := txHasColumn(ctx, tx, "titles", "fetch_error")
	if err != nil {
		return err
	}
	if oldSource, _, err := txHasColumn(ctx, tx, "tracker_accounts", "rating_source"); err != nil {
		return err
	} else if oldSource {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE tracker_accounts RENAME COLUMN rating_source TO source`); err != nil {
			return err
		}
	}
	if oldRelated, _, err := txHasColumn(ctx, tx, "tracker_related", "fetched_at"); err != nil {
		return err
	} else if oldRelated {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE tracker_related RENAME COLUMN fetched_at TO relations_fetched_at;
			ALTER TABLE tracker_related ADD COLUMN recommendations_fetched_at TEXT NOT NULL DEFAULT '';
			UPDATE tracker_related SET recommendations_fetched_at = relations_fetched_at`); err != nil {
			return err
		}
	}
	for _, col := range []struct{ table, name, def string }{
		{"jobs", "parent_id", "INTEGER"},
		{"catalog_manga", "is_adult", "INTEGER NOT NULL DEFAULT 0"},
		{"catalog_manga", "tags_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"users", "allow_adult", "INTEGER NOT NULL DEFAULT 0"},
		{"users", "blocked_tags", "TEXT NOT NULL DEFAULT '[]'"},
		{"users", "allowed_tags", "TEXT NOT NULL DEFAULT '[]'"},
		{"users", "blocked_providers", "TEXT NOT NULL DEFAULT '[]'"},
		{"tracker_accounts", "auto_link", "INTEGER NOT NULL DEFAULT 1"},
		{"tracker_accounts", "auto_progressive", "INTEGER NOT NULL DEFAULT 0"},
		{"tracker_accounts", "use_relations", "INTEGER NOT NULL DEFAULT 1"},
		{"tracker_accounts", "use_recommendations", "INTEGER NOT NULL DEFAULT 1"},
		{"tracker_accounts", "rating_sync", "TEXT NOT NULL DEFAULT ''"},
		{"tracker_accounts", "source", "TEXT NOT NULL DEFAULT 'kaodoku'"},
		{"tracker_accounts", "position", "INTEGER NOT NULL DEFAULT 0"},
		{"tracker_ratings", "choice", "TEXT NOT NULL DEFAULT ''"},
		{"tracker_accounts", "list_json", "TEXT NOT NULL DEFAULT ''"},
		{"tracker_accounts", "list_fetched_at", "TEXT NOT NULL DEFAULT ''"},
		{"tracker_title_sync", "enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"content_tags", "is_adult", "INTEGER NOT NULL DEFAULT 0"},
		{"content_tags", "category", "TEXT NOT NULL DEFAULT ''"},
		{"content_tags", "description", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "user_agent", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "ip", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "last_seen_at", "TEXT"},
		{"volumes", "thumb", "BLOB"},
		{"volumes", "thumb_type", "TEXT NOT NULL DEFAULT ''"},
		{"users", "oidc_subject", "TEXT NOT NULL DEFAULT ''"},
		{"users", "pending", "INTEGER NOT NULL DEFAULT 0"},
		{"titles", "cover", "BLOB"},
		{"titles", "cover_type", "TEXT NOT NULL DEFAULT ''"},
		{"volume_read_progress", "read_pages", "INTEGER NOT NULL DEFAULT 0"},
		{"volume_read_progress", "total_pages", "INTEGER NOT NULL DEFAULT 0"},
		{"volume_read_progress", "last_page", "INTEGER NOT NULL DEFAULT 0"},
		{"titles", "language_mode", "TEXT NOT NULL DEFAULT ''"},
		{"titles", "language_gap", "INTEGER NOT NULL DEFAULT 0"},
		{"titles", "added_by", "INTEGER NOT NULL DEFAULT 0"},
		{"library_screens", "position", "INTEGER NOT NULL DEFAULT 0"},
		{"catalog_manga", "synonyms_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"catalog_manga", "wanted", "INTEGER NOT NULL DEFAULT 0"},
		{"catalog_manga", "volumes", "INTEGER"},
		{"catalog_manga", "genres_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"catalog_manga", "authors_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"catalog_manga", "year", "INTEGER NOT NULL DEFAULT 0"},
		{"catalog_manga", "average_score", "INTEGER NOT NULL DEFAULT 0"},
		{"catalog_manga", "banner_image", "TEXT NOT NULL DEFAULT ''"},
		{"downloads", "attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"downloads", "pages", "INTEGER NOT NULL DEFAULT 0"},
		{"title_source_matches", "title", "TEXT NOT NULL DEFAULT ''"},
		{"title_source_matches", "chapters_found", "INTEGER NOT NULL DEFAULT 0"},
		{"title_source_matches", "sample_images_found", "INTEGER NOT NULL DEFAULT 0"},
		{"title_source_matches", "error", "TEXT NOT NULL DEFAULT ''"},
		{"title_source_matches", "updated_at", "TEXT NOT NULL DEFAULT ''"},
		{"api_tokens", "expires_at", "TEXT NOT NULL DEFAULT ''"},
		{"api_tokens", "last_seen_at", "TEXT NOT NULL DEFAULT ''"},
		{"api_tokens", "device_id", "TEXT NOT NULL DEFAULT ''"},
		{"chapter_read_progress", "manual", "INTEGER NOT NULL DEFAULT 0"}, // 1 = bulk/tracker mark, not page-by-page reading
		{"titles", "fetch_error", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err = ensureColumn(ctx, tx, col.table, col.name, col.def); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", col.table, col.name, err)
		}
	}

	if _, err = tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc_subject ON users(oidc_subject) WHERE oidc_subject != ''`); err != nil {
		return fmt.Errorf("migrate users oidc index: %w", err)
	}
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_jobs_active_title ON jobs(json_extract(payload_json, '$.title_id')) WHERE status IN ('queued', 'running', 'failed')`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_active_payload ON jobs(payload_json, type) WHERE status IN ('queued', 'running', 'failed')`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_parent ON jobs(parent_id) WHERE parent_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_finished ON jobs(updated_at) WHERE status IN ('done', 'cancelled', 'dead')`,
		`CREATE INDEX IF NOT EXISTS idx_titles_name ON titles(display_title COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_titles_source ON titles(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_title_sources_source ON title_sources(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_titles_created ON titles(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_titles_updated ON titles(updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_downloads_completed_at ON downloads(completed_at) WHERE completed_at IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_volumes_created ON volumes(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_chapter_read_recent ON chapter_read_progress(user_id, last_read_at)`,
		`CREATE INDEX IF NOT EXISTS idx_volume_read_recent ON volume_read_progress(user_id, last_read_at)`,
	} {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate indexes: %w", err)
		}
	}

	if !hadManual {
		if _, err = tx.ExecContext(ctx, `
			UPDATE chapter_read_progress SET manual = 1
			WHERE completed = 1 AND (user_id, chapter_id) NOT IN (
				SELECT user_id, chapter_id FROM chapter_read_pages
				GROUP BY user_id, chapter_id HAVING COUNT(DISTINCT read_at) >= 2
			)`); err != nil {
			return fmt.Errorf("backfill read/marked flag: %w", err)
		}
	}
	if !hadFetchError {
		if _, err = tx.ExecContext(ctx, `UPDATE sources SET domains_json = '[]' WHERE scraper LIKE 'kotatsu:%'`); err != nil {
			return fmt.Errorf("clear kotatsu snapshot domains: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO provider_tags (provider, name, kind, category, description, is_adult)
		SELECT 'anilist', name, kind, category, description, is_adult FROM content_tags; DELETE FROM content_tags`); err != nil {
		return fmt.Errorf("migrate content tags: %w", err)
	}
	if err = migrateAniListAccounts(ctx, tx); err != nil {
		return fmt.Errorf("migrate anilist accounts: %w", err)
	}
	var version int
	if err = tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version < 1 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM catalog_external_ids WHERE method = 'miss'; PRAGMA user_version = 1`); err != nil {
			return fmt.Errorf("retry title matches: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}

	return nil
}

func migrateAniListAccounts(ctx context.Context, tx *sql.Tx) error {
	if _, exists, err := txHasColumn(ctx, tx, "user_anilist", "user_id"); err != nil || !exists {
		return err
	}
	for _, stmt := range []string{
		`INSERT OR IGNORE INTO tracker_accounts (user_id, provider, access_token, remote_user_id, remote_name, expires_at, created_at)
			SELECT user_id, 'anilist', access_token, CAST(anilist_user_id AS TEXT), anilist_name, expires_at, created_at FROM user_anilist`,
		`INSERT OR IGNORE INTO tracker_title_sync (user_id, title_id, provider)
			SELECT DISTINCT rp.user_id, c.title_id, 'anilist' FROM chapter_read_progress rp
			JOIN chapters c ON c.id = rp.chapter_id JOIN user_anilist ua ON ua.user_id = rp.user_id`,
		`DROP TABLE user_anilist`,
		`UPDATE jobs SET type = 'sync_trackers' WHERE type = 'sync_anilist'`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, def string) error {
	ok, _, err := txHasColumn(ctx, tx, table, column)
	if err != nil || ok {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def))
	return err
}

func txHasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()

	var exists bool
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, false, err
		}
		exists = true
		if name == column {
			return true, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	return false, exists, nil
}

// migrateReadTablesPerUser rebuilds the read-progress tables with a user_id
// key; pre-existing single-user progress is assigned to the env admin (id 1).
func migrateReadTablesPerUser(ctx context.Context, tx *sql.Tx) error {
	hasUser, exists, err := txHasColumn(ctx, tx, "chapter_read_progress", "user_id")
	if err != nil || !exists || hasUser {
		return err
	}
	stmts := []string{
		`CREATE TABLE chapter_read_progress_new (
			user_id INTEGER NOT NULL DEFAULT 1,
			chapter_id INTEGER NOT NULL REFERENCES chapters(id) ON DELETE CASCADE,
			last_page INTEGER NOT NULL DEFAULT 0,
			read_pages INTEGER NOT NULL DEFAULT 0,
			total_pages INTEGER NOT NULL DEFAULT 0,
			completed INTEGER NOT NULL DEFAULT 0,
			started_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_read_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			completed_at TEXT,
			PRIMARY KEY (user_id, chapter_id)
		)`,
		`INSERT INTO chapter_read_progress_new (user_id, chapter_id, last_page, read_pages, total_pages, completed, started_at, last_read_at, completed_at)
			SELECT 1, chapter_id, last_page, read_pages, total_pages, completed, started_at, last_read_at, completed_at FROM chapter_read_progress`,
		`DROP TABLE chapter_read_progress`,
		`ALTER TABLE chapter_read_progress_new RENAME TO chapter_read_progress`,
		`CREATE TABLE chapter_read_pages_new (
			user_id INTEGER NOT NULL DEFAULT 1,
			chapter_id INTEGER NOT NULL REFERENCES chapters(id) ON DELETE CASCADE,
			page INTEGER NOT NULL,
			read_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, chapter_id, page)
		)`,
		`INSERT INTO chapter_read_pages_new (user_id, chapter_id, page, read_at)
			SELECT 1, chapter_id, page, read_at FROM chapter_read_pages`,
		`DROP TABLE chapter_read_pages`,
		`ALTER TABLE chapter_read_pages_new RENAME TO chapter_read_pages`,
		`CREATE INDEX IF NOT EXISTS idx_chapter_read_pages_chapter_id ON chapter_read_pages(chapter_id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate read tables per-user: %w", err)
		}
	}
	return nil
}
