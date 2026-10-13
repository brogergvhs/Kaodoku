package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

func TestZTmpImp(t *testing.T) {
	live := os.Getenv("KAODOKU_KOTATSU_LIVE")
	if live == "" {
		t.Skip("no live worker")
	}
	endpoint, key, _ := strings.Cut(live, "|")
	ctx := context.Background()
	svc, closeDB, err := service.OpenJobs(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	svc.SetRuntimeConfig(func() (*config.Config, ui.Log, error) {
		cfg := config.DefaultConfig()
		cfg.KotatsuWorker = config.KotatsuWorkerConfig{Enabled: true, Endpoint: endpoint, APIKey: key}
		return cfg, ui.New(ui.Options{Writer: io.Discard}), nil
	})
	api := New(svc,
		func(context.Context) (service.RunSummary, error) { return service.RunSummary{}, nil },
		func(context.Context, string) (service.SourceVerifyResult, error) {
			return service.SourceVerifyResult{}, nil
		})

	for _, group := range strings.Split(os.Getenv("ZGROUPS"), ";") {
		form := url.Values{}
		for _, id := range strings.Split(group, ",") {
			form.Add("id", id)
		}
		req := httptest.NewRequest(http.MethodPost, "/ui/sources/kotatsu/import", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		start := time.Now()
		api.ServeHTTP(rec, req)
		t.Logf("import %-28s %6.1fs (status %d)", group, time.Since(start).Seconds(), rec.Code)
	}
}
