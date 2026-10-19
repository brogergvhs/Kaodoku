// Package database opens and migrates the local Kaodoku SQLite database.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/config"

	_ "modernc.org/sqlite"
)

// DefaultPath returns the default application database path.
func DefaultPath() string {
	if path := os.Getenv("KAODOKU_DB"); path != "" {
		return path
	}
	return filepath.Join(config.ConfigRoot(), "kaodoku.db")
}

// Open opens a SQLite database and applies connection-level pragmas.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// DSN pragmas apply to every pooled connection; executing PRAGMA
	// statements on the *sql.DB would only configure one connection.
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	return db, nil
}

type Handle interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// DB keeps writes on the single-connection writer and sends plain SELECTs to a read-only pool.
type DB struct {
	*sql.DB
	read *sql.DB
}

func WithReaders(ctx context.Context, w *sql.DB, path string) (*DB, error) {
	r, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite readers: %w", err)
	}
	if err := r.PingContext(ctx); err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("open sqlite readers: %w", err)
	}
	n := max(4, runtime.NumCPU())
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)
	return &DB{DB: w, read: r}, nil
}

func (db *DB) Close() error {
	return errors.Join(db.read.Close(), db.DB.Close())
}

func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if isRead(query) {
		return db.read.QueryContext(ctx, query, args...)
	}
	return db.DB.QueryContext(ctx, query, args...)
}

func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if isRead(query) {
		return db.read.QueryRowContext(ctx, query, args...)
	}
	return db.DB.QueryRowContext(ctx, query, args...)
}

func isRead(query string) bool {
	query = strings.TrimLeft(query, " \t\r\n")
	return len(query) >= 6 && strings.EqualFold(query[:6], "SELECT")
}
