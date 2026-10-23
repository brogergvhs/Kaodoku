package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var userDataTables = []string{
	"roles",
	"users",
	"settings",
	"user_settings",
	"api_tokens",
	"tracker_accounts",
	"sources",
	"catalog_manga",
	"catalog_external_ids",
	"mangabaka_suggestions",
	"titles",
	"title_sources",
	"chapters",
	"chapter_read_progress",
	"chapter_read_pages",
	"volumes",
	"volume_read_progress",
	"collections",
	"collection_members",
	"collection_smart_pins",
	"library_screens",
	"user_favourites",
	"tracker_title_sync",
	"user_ratings",
	"tracker_ratings",
}

var restoreClearOnlyTables = []string{
	"jobs",
	"downloads",
	"sessions",
	"browser_cookies",
	"title_source_matches",
	"catalog_relations",
	"content_tags",
}

func BackupUserData(ctx context.Context, dbPath, outPath string) error {
	if outPath == "" {
		return errors.New("backup path is required")
	}
	if dbPath == "" {
		dbPath = DefaultPath()
	}
	if _, err := os.Stat(dbPath); err != nil {
		return err
	}
	tmp, err := tempDBPath(outPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(tmp)
	}()
	db, err := Open(ctx, tmp)
	if err != nil {
		return err
	}
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return err
	}
	if err := attach(ctx, db, "src", dbPath); err != nil {
		_ = db.Close()
		return err
	}
	if err := copyUserTables(ctx, db, "src", false); err != nil {
		_ = db.Close()
		return err
	}
	if _, err := db.ExecContext(ctx, `DETACH DATABASE src`); err != nil {
		_ = db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, outPath)
}

func RestoreUserData(ctx context.Context, dbPath, inPath string) error {
	if inPath == "" {
		return errors.New("backup path is required")
	}
	if dbPath == "" {
		dbPath = DefaultPath()
	}
	if _, err := os.Stat(inPath); err != nil {
		return err
	}
	db, err := Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		return err
	}
	if err := attach(ctx, db, "src", inPath); err != nil {
		return err
	}
	if err := copyUserTables(ctx, db, "src", true); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DETACH DATABASE src`)
	return err
}

func copyUserTables(ctx context.Context, db *sql.DB, src string, replace bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if replace {
		for i := len(restoreClearOnlyTables) - 1; i >= 0; i-- {
			if _, err = tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(restoreClearOnlyTables[i])); err != nil {
				return err
			}
		}
		for i := len(userDataTables) - 1; i >= 0; i-- {
			if _, err = tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(userDataTables[i])); err != nil {
				return err
			}
		}
	}
	for _, table := range userDataTables {
		cols, err := sharedColumns(ctx, tx, "main", src, table)
		if err != nil {
			return err
		}
		if len(cols) == 0 {
			continue
		}
		list := quoteList(cols)
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s`, quoteIdent(table), list, list, qualified(src, table))); err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func sharedColumns(ctx context.Context, tx *sql.Tx, dst, src, table string) ([]string, error) {
	want, err := tableColumns(ctx, tx, dst, table)
	if err != nil {
		return nil, err
	}
	have, err := tableColumns(ctx, tx, src, table)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, name := range have {
		seen[name] = true
	}
	var out []string
	for _, name := range want {
		if seen[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, schema, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA %s.table_info(%s)`, quoteIdent(schema), quoteString(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func attach(ctx context.Context, db *sql.DB, schema, path string) error {
	_, err := db.ExecContext(ctx, `ATTACH DATABASE ? AS `+quoteIdent(schema), path)
	return err
}

func tempDBPath(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	return name, f.Close()
}

func qualified(schema, table string) string {
	return quoteIdent(schema) + "." + quoteIdent(table)
}

func quoteList(cols []string) string {
	out := make([]string, len(cols))
	for i, col := range cols {
		out[i] = quoteIdent(col)
	}
	return strings.Join(out, ", ")
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteString(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
