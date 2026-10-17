package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/database"
)

func TestRepositoryQueueLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	repo := NewRepository(db)
	future, err := repo.Enqueue(ctx, TypeScanDownloads, `{"title_id":2}`, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Enqueue(future) error = %v", err)
	}
	due, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":1}`, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Enqueue(due) error = %v", err)
	}

	job, ok, err := repo.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if !ok {
		t.Fatal("ClaimNext() ok = false, want true")
	}
	if job.ID != due.ID || job.Status != "running" || job.Attempts != 1 {
		t.Fatalf("ClaimNext() = %+v, want due running attempt 1", job)
	}

	if err := repo.MarkDone(ctx, job.ID); err != nil {
		t.Fatalf("MarkDone() error = %v", err)
	}
	done, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get(done) error = %v", err)
	}
	if done.Status != "done" {
		t.Fatalf("done status = %q, want done", done.Status)
	}

	_, ok, err = repo.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext(no due) error = %v", err)
	}
	if ok {
		t.Fatal("ClaimNext(no due) ok = true, want false")
	}

	// Outcome marks only apply to running jobs.
	if err := repo.MarkFailed(ctx, future.ID, assertErr("boom")); err == nil {
		t.Fatal("MarkFailed(queued job) error = nil, want not-running error")
	}
	jobs, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("List() len = %d, want 2", len(jobs))
	}
}

func TestRepositoryRetryBackoffAndCap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	repo := NewRepository(db)
	job, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":1}`, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	for attempt := 1; attempt <= DefaultMaxAttempts; attempt++ {
		claimed, ok, err := repo.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("ClaimNext(%d) error = %v", attempt, err)
		}
		if !ok {
			t.Fatalf("ClaimNext(%d) ok = false", attempt)
		}
		if err := repo.MarkFailed(ctx, claimed.ID, assertErr("boom")); err != nil {
			t.Fatalf("MarkFailed(%d) error = %v", attempt, err)
		}
		got, err := repo.Get(ctx, job.ID)
		if err != nil {
			t.Fatalf("Get(%d) error = %v", attempt, err)
		}
		wantStatus := "failed"
		if attempt == DefaultMaxAttempts {
			wantStatus = "dead"
		}
		if got.Status != wantStatus {
			t.Fatalf("attempt %d status = %q, want %q", attempt, got.Status, wantStatus)
		}
		if attempt < DefaultMaxAttempts && !got.RunAfter.After(time.Now()) {
			t.Fatalf("attempt %d run_after = %s, want future retry", attempt, got.RunAfter)
		}
		_, _ = db.ExecContext(ctx, `UPDATE jobs SET run_after = CURRENT_TIMESTAMP WHERE id = ?`, job.ID)
	}

	_, ok, err := repo.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext(dead) error = %v", err)
	}
	if ok {
		t.Fatal("ClaimNext(dead) ok = true, want false")
	}
}

type assertErr string

func (e assertErr) Error() string {
	return string(e)
}

func TestRepositoryReconcileRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	repo := NewRepository(db)
	stranded, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":1}`, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Enqueue(stranded) error = %v", err)
	}
	exhausted, err := repo.Enqueue(ctx, TypeScanDownloads, `{"title_id":2}`, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Enqueue(exhausted) error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status = 'running' WHERE id = ?`, stranded.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status = 'running', attempts = ? WHERE id = ?`, DefaultMaxAttempts, exhausted.ID); err != nil {
		t.Fatal(err)
	}

	changed, err := repo.ReconcileRunning(ctx)
	if err != nil {
		t.Fatalf("ReconcileRunning() error = %v", err)
	}
	if changed != 2 {
		t.Fatalf("ReconcileRunning() = %d, want 2", changed)
	}

	requeued, err := repo.Get(ctx, stranded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != "queued" || requeued.LastError != "interrupted" {
		t.Fatalf("stranded job = %q/%q, want queued/interrupted", requeued.Status, requeued.LastError)
	}
	dead, err := repo.Get(ctx, exhausted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dead.Status != "dead" {
		t.Fatalf("exhausted job status = %q, want dead", dead.Status)
	}
}

func TestRepositoryEnqueueDedupesPendingJobs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	repo := NewRepository(db)
	later, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":1}`, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	same, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":1}`, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(duplicate) error = %v", err)
	}
	if same.ID != later.ID {
		t.Fatalf("duplicate enqueue ID = %d, want %d", same.ID, later.ID)
	}
	if !same.RunAfter.Before(later.RunAfter) {
		t.Fatalf("run_after not moved forward: %s vs %s", same.RunAfter, later.RunAfter)
	}

	other, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":2}`, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(other payload) error = %v", err)
	}
	if other.ID == later.ID {
		t.Fatal("different payload reused the same job")
	}

	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status = 'done' WHERE id = ?`, later.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":1}`, time.Now())
	if err != nil {
		t.Fatalf("Enqueue(after done) error = %v", err)
	}
	if fresh.ID == later.ID {
		t.Fatal("done job was reused instead of a fresh enqueue")
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)

	// A queued job cancels directly.
	q, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":1}`, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := repo.Cancel(ctx, q.ID)
	if err != nil || !ok {
		t.Fatalf("Cancel(queued) ok=%v err=%v", ok, err)
	}
	if got, _ := repo.Get(ctx, q.ID); got.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}

	// A running job is not cancellable via Cancel (aborted by context instead).
	r2, _ := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":2}`, time.Now().Add(-time.Minute))
	if _, _, err := repo.ClaimNext(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err = repo.Cancel(ctx, r2.ID)
	if err != nil || ok {
		t.Fatalf("Cancel(running) ok=%v err=%v, want false", ok, err)
	}
	// MarkCancelled works on the running job.
	if err := repo.MarkCancelled(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(ctx, r2.ID); got.Status != "cancelled" {
		t.Fatalf("running status = %q, want cancelled", got.Status)
	}
}

func TestCancelChildrenCascade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)

	parent, _ := repo.Enqueue(ctx, TypeDownloadMissing, `{}`, time.Now().Add(-time.Minute))
	c1, _ := repo.EnqueueChild(ctx, TypeDownloadMissing, `{"title_id":1}`, time.Now().Add(time.Hour), parent.ID)
	c2, _ := repo.EnqueueChild(ctx, TypeDownloadMissing, `{"title_id":2}`, time.Now().Add(-time.Minute), parent.ID)
	if c1.ParentID != parent.ID || c2.ParentID != parent.ID {
		t.Fatalf("children parent = %d/%d, want %d", c1.ParentID, c2.ParentID, parent.ID)
	}
	// claim c2 so it's running (parent is claimed first as it's oldest due)
	if _, _, err := repo.ClaimNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.ClaimNext(ctx); err != nil {
		t.Fatal(err)
	}
	running, err := repo.CancelChildren(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0] != c2.ID {
		t.Fatalf("running children = %v, want [%d]", running, c2.ID)
	}
	if got, _ := repo.Get(ctx, c1.ID); got.Status != "cancelled" {
		t.Fatalf("queued child status = %q, want cancelled", got.Status)
	}
}

func TestListNeverDropsActiveJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)

	running, _ := repo.Enqueue(ctx, TypeDownloadMissing, `{"title_id":7}`, time.Now().Add(-time.Minute))
	if _, _, err := repo.ClaimNext(ctx); err != nil {
		t.Fatal(err)
	}
	// bury it under 150 newer terminal rows
	for i := 0; i < 150; i++ {
		j, err := repo.Enqueue(ctx, TypeRefreshTitle, fmt.Sprintf(`{"title_id":%d}`, 1000+i), time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := repo.ClaimNext(ctx); err != nil {
			t.Fatal(err)
		}
		if err := repo.MarkDone(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range list {
		if j.ID == running.ID {
			if j.Status != "running" {
				t.Fatalf("status = %q", j.Status)
			}
			return
		}
	}
	t.Fatalf("running job %d missing from List (%d rows)", running.ID, len(list))
}

func TestActiveForTitle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)

	mine, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":7}`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A library-wide job omits title_id entirely, so it must still be seen.
	global, err := repo.Enqueue(ctx, TypeRefreshTitle, `{}`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Enqueue(ctx, TypeRefreshTitle, `{"title_id":8}`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Enqueue(ctx, TypeScanDownloads, `{"title_id":7}`, time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ActiveForTitle(ctx, TypeRefreshTitle, 7)
	if err != nil {
		t.Fatalf("ActiveForTitle() error = %v", err)
	}
	ids := map[int64]bool{}
	for _, j := range got {
		ids[j.ID] = true
	}
	if len(got) != 2 || !ids[mine.ID] || !ids[global.ID] {
		t.Fatalf("ActiveForTitle() = %v, want exactly jobs %d and %d", ids, mine.ID, global.ID)
	}

	if _, err := repo.Cancel(ctx, mine.ID); err != nil {
		t.Fatal(err)
	}
	got, err = repo.ActiveForTitle(ctx, TypeRefreshTitle, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != global.ID {
		t.Fatalf("after finishing, got %d jobs, want only the global one", len(got))
	}
}
