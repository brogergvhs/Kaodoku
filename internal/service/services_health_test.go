package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/config"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

func TestServicesHealthIncludesKotatsuWorker(t *testing.T) {
	t.Parallel()

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"parsers":3,"version":"test"}`))
	}))
	defer worker.Close()

	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	endpoint := worker.URL
	svc.SetRuntimeConfig(func() (*config.Config, ui.Log, error) {
		cfg := config.DefaultConfig()
		cfg.KotatsuWorker = config.KotatsuWorkerConfig{Enabled: true, Endpoint: endpoint}
		return cfg, ui.New(ui.Options{Writer: io.Discard}), nil
	})

	got := svc.ServicesHealth(ctx)
	if len(got) != 3+len(ProviderNames())-1 {
		t.Fatalf("services = %d, want 3 plus the configured trackers: %+v", len(got), got)
	}
	k := got[2]
	if k.Endpoint != endpoint || !k.Enabled || !k.Healthy || k.Version != "test" || k.Error != "" {
		t.Fatalf("kotatsu service = %+v", k)
	}

	worker.Close()
	if k := svc.ServicesHealth(ctx)[2]; k.Healthy || k.Error == "" {
		t.Fatalf("stopped worker should report unreachable: %+v", k)
	}
}
