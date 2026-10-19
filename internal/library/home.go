package library

import (
	"context"
	"strconv"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/database"
)

// LastReadAt returns at least the k most recently read titles; more reports whether older ones exist.
func (r *Repository) LastReadAt(ctx context.Context, k int) (map[int64]time.Time, bool, error) {
	uid := auth.UserID(ctx)
	out := map[int64]time.Time{}
	more := false
	for _, q := range []string{
		`SELECT c.title_id, '', rp.last_read_at FROM chapter_read_progress rp JOIN chapters c ON c.id = rp.chapter_id WHERE rp.user_id = ? ORDER BY rp.last_read_at DESC`,
		`SELECT v.title_id, '', vr.last_read_at FROM volume_read_progress vr JOIN volumes v ON v.id = vr.volume_id WHERE vr.user_id = ? ORDER BY vr.last_read_at DESC`,
	} {
		newest, cut, err := r.newestPerTitle(ctx, k, q, uid)
		if err != nil {
			return nil, false, err
		}
		more = more || cut
		for id, a := range newest {
			if cur, ok := out[id]; !ok || a.At.After(cur) {
				out[id] = a.At
			}
		}
	}
	return out, more, nil
}

// Arrival is a title's newest piece of content.
type Arrival struct {
	At    time.Time
	Label string // e.g. "Ch 145" or "Vol 12"
}

// LatestArrivals returns at least the k titles with the newest chapter download or volume file.
func (r *Repository) LatestArrivals(ctx context.Context, k int) (map[int64]Arrival, bool, error) {
	out, more, err := r.newestPerTitle(ctx, k, `
		SELECT c.title_id, 'Ch ' || c.label, d.completed_at
		FROM downloads d JOIN chapters c ON c.id = d.chapter_id
		WHERE d.status = 'completed' AND d.completed_at IS NOT NULL
		ORDER BY d.completed_at DESC`)
	if err != nil {
		return nil, false, err
	}
	vols, cut, err := r.newestPerTitle(ctx, k, `SELECT title_id, number, created_at FROM volumes ORDER BY created_at DESC`)
	if err != nil {
		return nil, false, err
	}
	for id, a := range vols {
		if cur, ok := out[id]; !ok || a.At.After(cur.At) {
			number, _ := strconv.ParseFloat(a.Label, 64)
			a.Label = volumeArrivalLabel(number)
			out[id] = a
		}
	}
	return out, more || cut, nil
}

func (r *Repository) newestPerTitle(ctx context.Context, k int, query string, args ...any) (map[int64]Arrival, bool, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := map[int64]Arrival{}
	for rows.Next() {
		var id int64
		var label, at string
		if err := rows.Scan(&id, &label, &at); err != nil {
			return nil, false, err
		}
		if _, seen := out[id]; seen {
			continue
		}
		if len(out) == k {
			return out, true, nil
		}
		if t, err := database.ParseTime(at); err == nil {
			out[id] = Arrival{At: t, Label: label}
		}
	}
	return out, false, rows.Err()
}

func volumeArrivalLabel(number float64) string {
	if number <= 0 {
		return "New volume"
	}
	return "Vol " + strconv.FormatFloat(number, 'f', -1, 64)
}
