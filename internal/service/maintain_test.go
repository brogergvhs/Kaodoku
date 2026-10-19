package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/database"
)

func TestMaintainPrunesPerSettingAndAnalyzes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	old := database.FormatTime(time.Now().Add(-1000 * time.Hour))
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO jobs(type, status, updated_at) VALUES ('scan_downloads', 'done', ?)`, old); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetSetting(ctx, SettingJobsKeepHistory, "0"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("Maintain with 0 kept %d jobs, %v", kept, err)
	}
	if err := svc.SetSetting(ctx, SettingJobsKeepHistory, "720h"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var jobsLeft, stats int
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&jobsLeft); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'sqlite_stat1'`).Scan(&stats); err != nil {
		t.Fatal(err)
	}
	if jobsLeft != 0 || stats != 1 {
		t.Fatalf("after Maintain: %d jobs, sqlite_stat1 present=%d", jobsLeft, stats)
	}
}
