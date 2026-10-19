package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const windowSQL = `(status IN ('queued', 'running', 'failed') OR id IN (SELECT id FROM jobs ORDER BY id DESC LIMIT 100))`

type Filter struct {
	TitleIDs []int64
	Type     string
	Statuses []string
	Payload  string
	BeforeID int64
	Limit    int
	Newest   bool
}

func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	if f.TitleIDs != nil {
		ids, _ := json.Marshal(f.TitleIDs)
		conds = append(conds, `json_extract(payload_json, '$.title_id') IN (SELECT value FROM json_each(?))`)
		args = append(args, string(ids))
	}
	if f.Type != "" {
		conds = append(conds, `type = ?`)
		args = append(args, f.Type)
	}
	if f.Payload != "" {
		conds = append(conds, `payload_json = ?`)
		args = append(args, f.Payload)
	}
	if len(f.Statuses) > 0 {
		conds = append(conds, `status IN (?`+strings.Repeat(", ?", len(f.Statuses)-1)+`)`)
		for _, s := range f.Statuses {
			args = append(args, s)
		}
	}
	if f.BeforeID > 0 {
		conds = append(conds, `id < ?`)
		args = append(args, f.BeforeID)
	}
	filter := ""
	for _, c := range conds {
		filter += ` AND ` + c
	}
	return `id IN (SELECT id FROM jobs WHERE status IN ('queued', 'running', 'failed')` + filter + `
		UNION SELECT id FROM (SELECT * FROM jobs ORDER BY id DESC LIMIT 100) WHERE 1 = 1` + filter + `)`, append(args, args...)
}

func (r *Repository) Find(ctx context.Context, f Filter) ([]Job, error) {
	where, args := f.where()
	if f.Newest {
		where = `id IN (SELECT MAX(id) FROM jobs WHERE ` + where + ` GROUP BY json_extract(payload_json, '$.title_id'), type, status)`
	}
	if f.Limit > 0 {
		where = fmt.Sprintf(`id IN (SELECT id FROM jobs WHERE %s ORDER BY id DESC LIMIT %d)`, where, f.Limit)
	}
	return r.query(ctx, jobSelect()+` WHERE `+where+` ORDER BY id DESC`, args...)
}

const topLevelSQL = `(parent_id IS NULL OR NOT EXISTS (SELECT 1 FROM jobs p WHERE p.id = jobs.parent_id))
	AND (` + windowSQL + ` OR id IN (SELECT parent_id FROM jobs WHERE parent_id IS NOT NULL AND ` + windowSQL + `))`

// TopLevel pages jobs without a live parent, including parents of windowed children.
func (r *Repository) TopLevel(ctx context.Context, sort, dir string, offset, limit int) ([]Job, int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE `+topLevelSQL).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count jobs: %w", err)
	}
	order := `id DESC`
	desc := dir == "desc"
	switch sort {
	case "type":
		order = map[bool]string{false: `type, id DESC`, true: `type DESC, id DESC`}[desc]
	case "updated":
		order = map[bool]string{false: `updated_at, id DESC`, true: `updated_at DESC, id DESC`}[desc]
	}
	list, err := r.query(ctx, jobSelect()+` WHERE id IN (SELECT id FROM jobs WHERE `+topLevelSQL+` ORDER BY `+order+` LIMIT ? OFFSET ?) ORDER BY `+order, limit, offset)
	return list, total, err
}

type ChildSummary struct {
	Counts map[string]int
	Total  int
	Newest []Job
}

func (r *Repository) Children(ctx context.Context, parentIDs []int64, newest int) (map[int64]*ChildSummary, error) {
	out := map[int64]*ChildSummary{}
	if len(parentIDs) == 0 {
		return out, nil
	}
	ids, _ := json.Marshal(parentIDs)
	in := `parent_id IN (SELECT value FROM json_each(?)) AND ` + windowSQL
	rows, err := r.db.QueryContext(ctx, `SELECT parent_id, status, COUNT(*) FROM jobs WHERE `+in+` GROUP BY parent_id, status`, string(ids))
	if err != nil {
		return nil, fmt.Errorf("count child jobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var status string
		var n int
		if err := rows.Scan(&id, &status, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = &ChildSummary{Counts: map[string]int{}}
		}
		out[id].Counts[status] += n
		out[id].Total += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list, err := r.query(ctx, `SELECT id, type, status, payload_json, run_after, attempts, last_error, parent_id, created_at, updated_at FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY parent_id ORDER BY id DESC) AS n FROM jobs WHERE `+in+`
		) WHERE n <= ? ORDER BY id DESC`, string(ids), newest)
	if err != nil {
		return nil, err
	}
	for _, j := range list {
		if s := out[j.ParentID]; s != nil {
			s.Newest = append(s.Newest, j)
		}
	}
	return out, nil
}

func (r *Repository) query(ctx context.Context, q string, args ...any) ([]Job, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}
