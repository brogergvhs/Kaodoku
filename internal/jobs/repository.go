package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/brogergvhs/kaodoku/internal/database"
)

const (
	DefaultMaxAttempts = 3
	baseBackoff        = time.Minute
	maxBackoff         = time.Hour
)

// Repository persists and claims background jobs.
type Repository struct {
	db database.Handle
	// MaxAttempts caps retries before a job becomes dead.
	MaxAttempts int
}

// NewRepository creates a jobs repository.
func NewRepository(db database.Handle) *Repository {
	return &Repository{db: db, MaxAttempts: DefaultMaxAttempts}
}

// Enqueue creates a queued job. An identical pending job is reused instead
// of duplicated; its run_after moves forward when an earlier one is asked.
func (r *Repository) Enqueue(ctx context.Context, typ string, payload string, runAfter time.Time) (Job, error) {
	return r.EnqueueChild(ctx, typ, payload, runAfter, 0)
}

// EnqueueChild enqueues a job linked to the global job that spawned it.
func (r *Repository) EnqueueChild(ctx context.Context, typ string, payload string, runAfter time.Time, parentID int64) (Job, error) {
	if runAfter.IsZero() {
		runAfter = time.Now()
	}

	var id int64
	err := r.db.QueryRowContext(ctx, `
		SELECT id FROM jobs
		WHERE type = ? AND payload_json = ? AND status IN ('queued', 'running', 'failed')
		ORDER BY id
		LIMIT 1
	`, typ, payload).Scan(&id)
	if err == nil {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE jobs
			SET run_after = MIN(run_after, ?), updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status != 'running'
		`, database.FormatTime(runAfter), id); err != nil {
			return Job{}, fmt.Errorf("reschedule pending job %d: %w", id, err)
		}
		return r.Get(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("find pending job: %w", err)
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO jobs(type, status, payload_json, run_after, parent_id)
		VALUES (?, 'queued', ?, ?, NULLIF(?, 0))
		RETURNING id
	`, typ, payload, database.FormatTime(runAfter), parentID)

	if err := row.Scan(&id); err != nil {
		return Job{}, fmt.Errorf("enqueue job: %w", err)
	}

	return r.Get(ctx, id)
}

// Get returns one job.
func (r *Repository) Get(ctx context.Context, id int64) (Job, error) {
	row := r.db.QueryRowContext(ctx, jobSelect()+` WHERE id = ?`, id)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, fmt.Errorf("job %d not found", id)
		}
		return Job{}, fmt.Errorf("get job %d: %w", id, err)
	}

	return job, nil
}

func (r *Repository) ActiveForTitle(ctx context.Context, typ string, titleID int64) ([]Job, error) {
	rows, err := r.db.QueryContext(ctx, jobSelect()+` WHERE type = ?
			AND status IN ('queued', 'running', 'failed')
			AND (json_extract(payload_json, '$.title_id') IS NULL
				OR json_extract(payload_json, '$.title_id') = ?)
			ORDER BY id DESC`, typ, titleID)
	if err != nil {
		return nil, fmt.Errorf("list active jobs for title %d: %w", titleID, err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active jobs: %w", err)
	}
	return out, nil
}

// List returns recent jobs.
func (r *Repository) List(ctx context.Context) ([]Job, error) {
	// The window covers recent history, but active jobs must never fall out
	// of sight — a burst of newer rows (e.g. a sweep expanding into hundreds
	// of children) must not hide a still-running job from the UI.
	rows, err := r.db.QueryContext(ctx, jobSelect()+` WHERE id IN (
			SELECT id FROM jobs WHERE status IN ('queued', 'running', 'failed')
			UNION
			SELECT id FROM (SELECT id FROM jobs ORDER BY id DESC LIMIT 100)
		) ORDER BY id DESC`)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}

	return out, nil
}

// ClaimNext marks the next due queued/failed job as running. The single
// guarded UPDATE makes the claim atomic without a write transaction.
func (r *Repository) ClaimNext(ctx context.Context) (Job, bool, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `
		UPDATE jobs
		SET status = 'running', attempts = attempts + 1, last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM jobs
			WHERE status IN ('queued', 'failed') AND run_after <= CURRENT_TIMESTAMP
			ORDER BY run_after, id
			LIMIT 1
		) AND status IN ('queued', 'failed')
		RETURNING id
	`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim job: %w", err)
	}

	job, err := r.Get(ctx, id)
	if err != nil {
		return Job{}, false, err
	}

	return job, true, nil
}

// ReconcileRunning requeues jobs left in running state by a process that
// exited before marking them; jobs already at the attempt cap become dead.
func (r *Repository) ReconcileRunning(ctx context.Context) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = CASE WHEN attempts >= ? THEN 'dead' ELSE 'queued' END,
			last_error = 'interrupted',
			updated_at = CURRENT_TIMESTAMP
		WHERE status = 'running'
	`, r.MaxAttempts)
	if err != nil {
		return 0, fmt.Errorf("reconcile running jobs: %w", err)
	}

	return result.RowsAffected()
}

// MarkDone marks a job as completed.
func (r *Repository) MarkDone(ctx context.Context, id int64) error {
	return r.mark(ctx, id, "done", "")
}

// MarkFailed marks a job as failed.
func (r *Repository) MarkFailed(ctx context.Context, id int64, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	job, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	status := "failed"
	runAfter := time.Now().Add(jobBackoff(job.Attempts))
	if job.Attempts >= r.MaxAttempts {
		status = "dead"
		runAfter = job.RunAfter
	}
	return r.markWithRunAfter(ctx, id, status, msg, runAfter)
}

// MarkCancelled marks a running job as cancelled (terminal, no retry).
func (r *Repository) MarkCancelled(ctx context.Context, id int64) error {
	return r.mark(ctx, id, "cancelled", "cancelled by user")
}

// Cancel terminates a not-yet-running job (queued or awaiting retry) and
// reports whether one was cancelled. Running jobs are aborted via their
// context instead and return false here.
func (r *Repository) Cancel(ctx context.Context, id int64) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'cancelled', last_error = 'cancelled by user', updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN ('queued', 'failed')
	`, id)
	if err != nil {
		return false, fmt.Errorf("cancel job %d: %w", id, err)
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// CancelChildren cancels all not-yet-running children of a global job and
// returns the IDs of children that are currently running (the caller aborts
// those via their contexts).
func (r *Repository) CancelChildren(ctx context.Context, parentID int64) ([]int64, error) {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'cancelled', last_error = 'cancelled by user', updated_at = CURRENT_TIMESTAMP
		WHERE parent_id = ? AND status IN ('queued', 'failed')
	`, parentID); err != nil {
		return nil, fmt.Errorf("cancel children of %d: %w", parentID, err)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM jobs WHERE parent_id = ? AND status = 'running'`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list running children of %d: %w", parentID, err)
	}
	defer rows.Close()
	var running []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		running = append(running, id)
	}
	return running, rows.Err()
}

func (r *Repository) mark(ctx context.Context, id int64, status, msg string) error {
	return r.markWithRunAfter(ctx, id, status, msg, time.Time{})
}

func (r *Repository) markWithRunAfter(ctx context.Context, id int64, status, msg string, runAfter time.Time) error {
	runAfterSQL := "run_after"
	args := []any{status, msg}
	if !runAfter.IsZero() {
		runAfterSQL = "?"
		args = append(args, database.FormatTime(runAfter))
	}
	args = append(args, id)
	// Outcomes only apply to running jobs; anything else means the job was
	// reconciled or re-claimed since, and must not be overwritten.
	result, err := r.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = ?, last_error = ?, run_after = `+runAfterSQL+`, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'running'
	`, args...)
	if err != nil {
		return fmt.Errorf("mark job %d %s: %w", id, status, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check job %d %s: %w", id, status, err)
	}
	if rows == 0 {
		return fmt.Errorf("job %d is not running", id)
	}

	return nil
}

func jobBackoff(attempts int) time.Duration {
	backoff := baseBackoff
	for i := 1; i < attempts; i++ {
		backoff *= 2
		if backoff >= maxBackoff {
			return maxBackoff
		}
	}
	return backoff
}

func jobSelect() string {
	return `
		SELECT id, type, status, payload_json, run_after, attempts, last_error, COALESCE(parent_id, 0), created_at, updated_at
		FROM jobs
	`
}

func scanJob(row database.Scanner) (Job, error) {
	var job Job
	var runAfter, createdAt, updatedAt string
	if err := row.Scan(&job.ID, &job.Type, &job.Status, &job.Payload, &runAfter, &job.Attempts, &job.LastError, &job.ParentID, &createdAt, &updatedAt); err != nil {
		return Job{}, err
	}

	var err error
	if job.RunAfter, err = database.ParseTime(runAfter); err != nil {
		return Job{}, err
	}
	if job.CreatedAt, err = database.ParseTime(createdAt); err != nil {
		return Job{}, err
	}
	if job.UpdatedAt, err = database.ParseTime(updatedAt); err != nil {
		return Job{}, err
	}

	return job, nil
}
