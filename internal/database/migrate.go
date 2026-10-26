package database

import (
	"context"
	"database/sql"
	"fmt"
)

const schemaVersion = 1

const schema = `
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
	thumb BLOB,
	thumb_type TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(title_id, file)
);
CREATE INDEX IF NOT EXISTS idx_volumes_title ON volumes(title_id);

CREATE TABLE IF NOT EXISTS volume_read_progress (
	user_id INTEGER NOT NULL DEFAULT 1,
	volume_id INTEGER NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
	completed INTEGER NOT NULL DEFAULT 0,
	read_pages INTEGER NOT NULL DEFAULT 0,
	total_pages INTEGER NOT NULL DEFAULT 0,
	last_page INTEGER NOT NULL DEFAULT 0,
	last_read_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, volume_id)
);

CREATE TABLE IF NOT EXISTS library_screens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL DEFAULT 1,
	name TEXT NOT NULL,
	config_json TEXT NOT NULL DEFAULT '{}',
	position INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_library_screens_user ON library_screens(user_id);

CREATE TABLE IF NOT EXISTS user_favourites (
	user_id INTEGER NOT NULL DEFAULT 1,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, title_id)
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
	banner_image TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(provider, provider_id)
);
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
	languages_json TEXT NOT NULL DEFAULT '[]',
	nsfw INTEGER NOT NULL DEFAULT 0,
	chapterless INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
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
	cover BLOB,
	cover_type TEXT NOT NULL DEFAULT '',
	language_mode TEXT NOT NULL DEFAULT '',
	language_gap INTEGER NOT NULL DEFAULT 0,
	added_by INTEGER NOT NULL DEFAULT 0,
	fetch_error TEXT NOT NULL DEFAULT '',
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
	volume_custom INTEGER NOT NULL DEFAULT 0,
	source_uid TEXT NOT NULL DEFAULT '',
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
	manual INTEGER NOT NULL DEFAULT 0,
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
	blocked_tags TEXT NOT NULL DEFAULT '[]',
	allowed_tags TEXT NOT NULL DEFAULT '[]',
	blocked_providers TEXT NOT NULL DEFAULT '[]',
	oidc_subject TEXT NOT NULL DEFAULT '',
	pending INTEGER NOT NULL DEFAULT 0,
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
	auto_link INTEGER NOT NULL DEFAULT 1,
	auto_progressive INTEGER NOT NULL DEFAULT 0,
	use_relations INTEGER NOT NULL DEFAULT 1,
	use_recommendations INTEGER NOT NULL DEFAULT 1,
	rating_sync TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT 'kaodoku',
	position INTEGER NOT NULL DEFAULT 0,
	exclusive INTEGER NOT NULL DEFAULT 0,
	list_json TEXT NOT NULL DEFAULT '',
	list_fetched_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, provider)
);

CREATE TABLE IF NOT EXISTS tracker_title_sync (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	PRIMARY KEY (user_id, title_id, provider)
);

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
	choice TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (user_id, title_id, provider)
);

CREATE TABLE IF NOT EXISTS tracker_synced (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title_id INTEGER NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	progress TEXT NOT NULL DEFAULT '',
	rating TEXT NOT NULL DEFAULT '',
	favourite TEXT NOT NULL DEFAULT '',
	remote TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (user_id, title_id, provider)
);

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
	expires_at TEXT NOT NULL DEFAULT '',
	last_seen_at TEXT NOT NULL DEFAULT '',
	device_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	user_agent TEXT NOT NULL DEFAULT '',
	ip TEXT NOT NULL DEFAULT '',
	last_seen_at TEXT
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
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc_subject ON users(oidc_subject) WHERE oidc_subject != '';
CREATE INDEX IF NOT EXISTS idx_jobs_active_title ON jobs(json_extract(payload_json, '$.title_id')) WHERE status IN ('queued', 'running', 'failed');
CREATE INDEX IF NOT EXISTS idx_jobs_active_payload ON jobs(payload_json, type) WHERE status IN ('queued', 'running', 'failed');
CREATE INDEX IF NOT EXISTS idx_jobs_parent ON jobs(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_jobs_finished ON jobs(updated_at) WHERE status IN ('done', 'cancelled', 'dead');
CREATE INDEX IF NOT EXISTS idx_titles_name ON titles(display_title COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_titles_source ON titles(source_id);
CREATE INDEX IF NOT EXISTS idx_title_sources_source ON title_sources(source_id);
CREATE INDEX IF NOT EXISTS idx_titles_created ON titles(created_at);
CREATE INDEX IF NOT EXISTS idx_titles_updated ON titles(updated_at);
CREATE INDEX IF NOT EXISTS idx_downloads_completed_at ON downloads(completed_at) WHERE completed_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_volumes_created ON volumes(created_at);
CREATE INDEX IF NOT EXISTS idx_chapter_read_recent ON chapter_read_progress(user_id, last_read_at);
CREATE INDEX IF NOT EXISTS idx_volume_read_recent ON volume_read_progress(user_id, last_read_at);
`

// Migrate creates the schema in a new database and refuses one written by an
// incompatible version.
func Migrate(ctx context.Context, db *sql.DB) error {
	var version, tables int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT user_version FROM pragma_user_version), (SELECT COUNT(*) FROM sqlite_master WHERE type = 'table')`).Scan(&version, &tables); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version != 0 || tables > 0 {
		return fmt.Errorf("database schema %d does not match this version of Kaodoku (%d): back up your user data with `kaodoku backup <file>`, move the database away, then `kaodoku restore <file>`", version, schemaVersion)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schema+fmt.Sprintf("PRAGMA user_version = %d;", schemaVersion)); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return tx.Commit()
}
