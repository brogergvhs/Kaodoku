package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReadersDoNotWaitForWriter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kaodoku.db")
	w, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, w); err != nil {
		t.Fatal(err)
	}
	db, err := WithReaders(ctx, w, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES ('a', '1')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE settings SET value = '2' WHERE key = 'a'`); err != nil {
		t.Fatal(err)
	}

	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var value string
	if err := db.QueryRowContext(readCtx, `SELECT value FROM settings WHERE key = 'a'`).Scan(&value); err != nil || value != "1" {
		t.Fatalf("read during write tx = %q, %v; want committed value 1", value, err)
	}
	if _, err := db.read.ExecContext(ctx, `DELETE FROM settings`); err == nil {
		t.Fatal("reader pool accepted a write")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := db.QueryRowContext(ctx, `INSERT INTO settings(key, value) VALUES ('b', '3') RETURNING key`).Scan(&key); err != nil || key != "b" {
		t.Fatalf("write through QueryRowContext = %q, %v", key, err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT value FROM settings WHERE key = 'a'`).Scan(&value); err != nil || value != "2" {
		t.Fatalf("read after commit = %q, %v; want 2", value, err)
	}
}
