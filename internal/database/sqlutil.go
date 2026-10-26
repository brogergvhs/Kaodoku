package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Scanner abstracts *sql.Row and *sql.Rows for shared scan helpers.
type Scanner interface {
	Scan(dest ...any) error
}

type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Each calls fn for every row of q, stopping at the first error fn returns.
func Each(ctx context.Context, db Querier, q string, args []any, fn func(Scanner) error) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Collect scans every row of q with scan; any error returns nothing.
func Collect[T any](ctx context.Context, db Querier, scan func(Scanner) (T, error), q string, args ...any) ([]T, error) {
	var out []T
	err := Each(ctx, db, q, args, func(r Scanner) error {
		v, err := scan(r)
		out = append(out, v)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Column reads the one column q selects.
func Column[T any](ctx context.Context, db Querier, q string, args ...any) ([]T, error) {
	return Collect(ctx, db, func(r Scanner) (v T, err error) { return v, r.Scan(&v) }, q, args...)
}

// ParseTime parses the timestamp formats SQLite stores; empty means zero.
func ParseTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("parse sqlite time %q", value)
}

// FormatTime renders a timestamp the way CURRENT_TIMESTAMP stores it, so
// lexicographic comparisons in SQL stay correct.
func FormatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05")
}

// BoolToInt converts a bool to SQLite's 0/1.
func BoolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
