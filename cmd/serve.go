package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/server"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/util"

	"github.com/spf13/cobra"
)

// warnDownloadDir surfaces a bad download directory at startup instead of
// letting every download/import/delete fail silently later.
func warnDownloadDir(cmd *cobra.Command) {
	cfg, _, err := runtimeConfig()
	if err != nil {
		return
	}
	dir, _ := filepath.Abs(cfg.DownloadDir)
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: download directory %q does not exist — downloads and imports will fail until it is created or mounted\n", dir)
		return
	}
	f, werr := os.CreateTemp(dir, ".kaodoku-write-*")
	if werr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: download directory %q is not writable: %v\n", dir, werr)
		return
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
}

var (
	flagServeDB            string
	flagServeAddr          string
	flagServeRefreshEvery  time.Duration
	flagServeScanEvery     time.Duration
	flagServeDownloadEvery time.Duration
	flagServeBackupEvery   time.Duration
	flagServeRunEvery      time.Duration
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the automatic Kaodoku job scheduler",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&flagServeDB, "db", "", "path to Kaodoku SQLite database")
	serveCmd.Flags().StringVar(&flagServeAddr, "addr", "127.0.0.1:8080", "HTTP API listen address")
	serveCmd.Flags().DurationVar(&flagServeRefreshEvery, "refresh-every", 0, "refresh schedule, e.g. 1h; 0 disables")
	serveCmd.Flags().DurationVar(&flagServeScanEvery, "scan-every", 0, "download file scan schedule, e.g. 30m; 0 disables")
	serveCmd.Flags().DurationVar(&flagServeDownloadEvery, "download-every", 0, "missing download schedule, e.g. 10m; 0 disables")
	serveCmd.Flags().DurationVar(&flagServeBackupEvery, "backup-every", 0, "user data backup schedule, e.g. 24h; 0 disables")
	serveCmd.Flags().DurationVar(&flagServeRunEvery, "run-every", 0, "job runner interval, e.g. 5s")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc, closeDB, err := service.OpenJobs(ctx, flagServeDB)
	if err != nil {
		return err
	}
	defer closeDB()
	svc.SetRuntimeConfig(runtimeConfig)
	warnDownloadDir(cmd)
	cleanupDownloadTemps()

	// Make built-in sources available immediately (registry sync stays manual).
	if err := svc.SyncSources(ctx, ""); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: sync built-in sources: %v\n", err)
	}

	if err := seedServeSetting(cmd, svc, ctx, "refresh-every", service.SettingServeRefreshEvery, flagServeRefreshEvery); err != nil {
		return err
	}
	if err := seedServeSetting(cmd, svc, ctx, "scan-every", service.SettingServeScanEvery, flagServeScanEvery); err != nil {
		return err
	}
	if err := seedServeSetting(cmd, svc, ctx, "download-every", service.SettingServeDownloadEvery, flagServeDownloadEvery); err != nil {
		return err
	}
	if err := seedServeSetting(cmd, svc, ctx, "backup-every", service.SettingServeBackupEvery, flagServeBackupEvery); err != nil {
		return err
	}
	if err := seedServeSetting(cmd, svc, ctx, "run-every", service.SettingServeRunEvery, flagServeRunEvery); err != nil {
		return err
	}

	httpSrv := &http.Server{Addr: flagServeAddr, Handler: server.New(
		svc,
		func(ctx context.Context) (service.RunSummary, error) {
			return runDue(ctx, svc)
		},
		func(ctx context.Context, sourceID string) (service.SourceVerifyResult, error) {
			cfg, logSvc, err := svc.RuntimeConfig(ctx)
			if err != nil {
				return service.SourceVerifyResult{}, err
			}
			return svc.VerifySource(ctx, cfg, logSvc, sourceID)
		},
	)}
	httpErr := make(chan error, 1)
	go func() {
		fmt.Printf("HTTP API listening on %s\n", flagServeAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		svc.FlushPushes(10 * time.Second)
		svc.FlushRequests(context.Background())
	}()

	go svc.WatchProviders(ctx)
	go svc.WatchMemory(ctx)
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if err := svc.Maintain(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "maintenance: %v\n", err)
			}
			timer.Reset(24 * time.Hour)
		}
	}()

	schedules := []*schedule{
		{key: service.SettingServeRefreshEvery, typ: jobs.TypeRefreshTitle},
		{key: service.SettingServeScanEvery, typ: jobs.TypeScanDownloads},
		{key: service.SettingServeDownloadEvery, typ: jobs.TypeDownloadMissing},
		{key: service.SettingServeSourceVerifyEvery, typ: jobs.TypeVerifySource},
		{key: service.SettingServeBackupEvery, typ: jobs.TypeBackupUserData},
		{key: service.SettingServeTrackerSyncEvery, typ: jobs.TypeSyncTrackers},
		{key: service.SettingServeCatalogEvery, typ: jobs.TypeCatalogRefresh},
		{key: service.SettingMangaBakaEvery, typ: jobs.TypeMangaBakaRefresh},
	}
	var runEvery time.Duration
	for {
		changed := false
		for _, sc := range schedules {
			every := serveDuration(svc, ctx, sc.key)
			if sc.typ == jobs.TypeMangaBakaRefresh {
				every = svc.MangaBakaEvery(ctx)
			}
			if every == sc.every {
				continue
			}
			sc.every, sc.next, changed = every, nextAfter(svc, ctx, sc.typ, every), true
			if sc.typ == jobs.TypeMangaBakaRefresh && !svc.MangaBakaReady() {
				sc.next = time.Now()
			}
		}
		oldRun := runEvery
		if runEvery = serveDuration(svc, ctx, service.SettingServeRunEvery); runEvery <= 0 {
			runEvery = fallbackDuration(service.SettingDefault(service.SettingServeRunEvery))
		}
		if changed || oldRun != runEvery {
			fmt.Print("Serving:")
			for _, sc := range schedules {
				fmt.Printf(" %s=%s", sc.typ, sc.every)
			}
			fmt.Printf(" run=%s\n", runEvery)
		}
		for _, sc := range schedules {
			if !nextDue(&sc.next, sc.every) {
				continue
			}
			if err := enqueueScheduled(ctx, svc, sc.typ); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "serve tick: %v\n", err)
			}
		}
		if _, err := runDue(ctx, svc); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "run due jobs: %v\n", err)
		}

		timer := time.NewTimer(runEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case err := <-httpErr:
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

type schedule struct {
	key, typ string
	every    time.Duration
	next     time.Time
}

func enqueueScheduled(ctx context.Context, svc *service.JobService, typ string) error {
	if typ == jobs.TypeVerifySource {
		_, err := svc.EnqueueSourceVerification(ctx, time.Now())
		return err
	}
	_, err := svc.Enqueue(ctx, typ, 0, time.Now())
	return err
}

func runDue(ctx context.Context, svc *service.JobService) (service.RunSummary, error) {
	cfg, logSvc, err := svc.RuntimeConfig(ctx)
	if err != nil {
		return service.RunSummary{}, err
	}
	summary, err := svc.RunDue(ctx, cfg, logSvc)
	if err != nil {
		return summary, err
	}
	if summary.Done != 0 || summary.Failed != 0 {
		fmt.Printf("Jobs run: %d done, %d failed\n", summary.Done, summary.Failed)
	}
	return summary, nil
}

func seedServeSetting(cmd *cobra.Command, svc *service.JobService, ctx context.Context, flagName, key string, value time.Duration) error {
	if !cmd.Flags().Changed(flagName) {
		return nil
	}
	return svc.SetSetting(ctx, key, value.String())
}

func cleanupDownloadTemps() {
	cfg, _, err := runtimeConfig()
	if err == nil {
		util.CleanupUnfinishedTempFolders(cfg.DownloadDir)
	}
}

func serveDuration(svc *service.JobService, ctx context.Context, key string) time.Duration {
	fallback := service.SettingDefault(key)
	value := svc.Setting(ctx, key, fallback)
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallbackDuration(fallback)
	}
	return d
}

func nextAfter(svc *service.JobService, ctx context.Context, typ string, every time.Duration) time.Time {
	if last := svc.LastJobTime(ctx, typ); !last.IsZero() {
		return last.Add(every)
	}
	return time.Now()
}

func nextDue(next *time.Time, every time.Duration) bool {
	if every <= 0 || time.Now().Before(*next) {
		return false
	}
	*next = time.Now().Add(every)
	return true
}

func fallbackDuration(value string) time.Duration {
	d, _ := time.ParseDuration(value)
	return d
}
