package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

func TestRunDueOverlappingCallsShareWorkerBudget(t *testing.T) {
	t.Parallel()

	var inflight, peak atomic.Int64
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(kotatsu.Chapters{ResolvedURL: r.URL.Query().Get("url"), Chapters: []kotatsu.Chapter{{URL: "c1", Number: "1"}}})
	}))
	defer worker.Close()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	svc.jobWorkers = 2
	cfg := config.DefaultConfig()
	cfg.RateLimit.Disabled = true
	cfg.KotatsuWorker = config.KotatsuWorkerConfig{Enabled: true, Endpoint: worker.URL}
	if err := svc.ImportLocalSource(ctx, sources.Profile{ID: "fixture", Name: "Fixture", BaseURL: worker.URL, SampleMangaURL: worker.URL + "/title-0", Scraper: "kotatsu:fixture", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	const titles = 12
	for i := range titles {
		title, err := svc.lib.repo.AddTitle(ctx, library.AddTitleParams{SourceID: "fixture", SourceURL: fmt.Sprintf("%s/title-%d", worker.URL, i), DisplayTitle: fmt.Sprintf("T%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Enqueue(ctx, jobs.TypeRefreshTitle, title.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	var done, failed atomic.Int64
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sum, err := svc.RunDue(ctx, cfg, ui.New(ui.Options{Writer: io.Discard}))
			if err != nil {
				t.Error(err)
			}
			done.Add(int64(sum.Done))
			failed.Add(int64(sum.Failed))
		}()
	}
	wg.Wait()

	if done.Load() != titles || failed.Load() != 0 {
		t.Fatalf("done/failed = %d/%d, want %d/0", done.Load(), failed.Load(), titles)
	}
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak concurrent jobs = %d, want <= 2", p)
	}
}
