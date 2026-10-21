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
	}()

	go svc.WatchProviders(ctx)
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

	nextRefresh := time.Now()
	nextScan := time.Now()
	nextDownload := time.Now()
	nextSourceVerify := time.Now()
	nextBackup := time.Now()
	nextAniList := time.Now()
	nextCatalog := time.Now()
	var refreshEvery, scanEvery, downloadEvery, sourceVerifyEvery, backupEvery, anilistEvery, catalogEvery, runEvery time.Duration

	for {
		oldRefresh, oldScan, oldDownload, oldSourceVerify, oldBackup, oldAniList, oldCatalog, oldRun := refreshEvery, scanEvery, downloadEvery, sourceVerifyEvery, backupEvery, anilistEvery, catalogEvery, runEvery
		refreshEvery = serveDuration(svc, ctx, service.SettingServeRefreshEvery)
		scanEvery = serveDuration(svc, ctx, service.SettingServeScanEvery)
		downloadEvery = serveDuration(svc, ctx, service.SettingServeDownloadEvery)
		sourceVerifyEvery = serveDuration(svc, ctx, service.SettingServeSourceVerifyEvery)
		backupEvery = serveDuration(svc, ctx, service.SettingServeBackupEvery)
		anilistEvery = serveDuration(svc, ctx, service.SettingServeTrackerSyncEvery)
		catalogEvery = serveDuration(svc, ctx, service.SettingServeCatalogEvery)
		runEvery = serveDuration(svc, ctx, service.SettingServeRunEvery)
		if runEvery <= 0 {
			runEvery = fallbackDuration(service.SettingDefault(service.SettingServeRunEvery))
		}
		changed := oldRefresh != refreshEvery || oldScan != scanEvery || oldDownload != downloadEvery || oldSourceVerify != sourceVerifyEvery || oldBackup != backupEvery || oldAniList != anilistEvery || oldCatalog != catalogEvery || oldRun != runEvery
		if oldRefresh != refreshEvery {
			nextRefresh = nextAfter(svc, ctx, jobs.TypeRefreshTitle, refreshEvery)
		}
		if oldScan != scanEvery {
			nextScan = nextAfter(svc, ctx, jobs.TypeScanDownloads, scanEvery)
		}
		if oldDownload != downloadEvery {
			nextDownload = nextAfter(svc, ctx, jobs.TypeDownloadMissing, downloadEvery)
		}
		if oldSourceVerify != sourceVerifyEvery {
			nextSourceVerify = nextAfter(svc, ctx, jobs.TypeVerifySource, sourceVerifyEvery)
		}
		if oldBackup != backupEvery {
			nextBackup = nextAfter(svc, ctx, jobs.TypeBackupUserData, backupEvery)
		}
		if oldAniList != anilistEvery {
			nextAniList = nextAfter(svc, ctx, jobs.TypeSyncTrackers, anilistEvery)
		}
		if oldCatalog != catalogEvery {
			nextCatalog = nextAfter(svc, ctx, jobs.TypeCatalogRefresh, catalogEvery)
		}
		if changed {
			fmt.Printf("Serving: refresh=%s scan=%s download=%s source_verify=%s backup=%s run=%s\n", refreshEvery, scanEvery, downloadEvery, sourceVerifyEvery, backupEvery, runEvery)
		}

		if err := serveTick(ctx, svc, nextDue(&nextRefresh, refreshEvery), nextDue(&nextScan, scanEvery), nextDue(&nextDownload, downloadEvery), nextDue(&nextSourceVerify, sourceVerifyEvery), nextDue(&nextBackup, backupEvery), nextDue(&nextAniList, anilistEvery), nextDue(&nextCatalog, catalogEvery)); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "serve tick: %v\n", err)
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

func serveTick(ctx context.Context, svc *service.JobService, refresh, scan, download, sourceVerify, backup, anilist, catalogRefresh bool) error {
	now := time.Now()
	if refresh {
		if _, err := svc.Enqueue(ctx, jobs.TypeRefreshTitle, 0, now); err != nil {
			return err
		}
	}
	if scan {
		if _, err := svc.Enqueue(ctx, jobs.TypeScanDownloads, 0, now); err != nil {
			return err
		}
	}
	if download {
		if _, err := svc.Enqueue(ctx, jobs.TypeDownloadMissing, 0, now); err != nil {
			return err
		}
	}
	if sourceVerify {
		if _, err := svc.EnqueueSourceVerification(ctx, now); err != nil {
			return err
		}
	}
	if backup {
		if _, err := svc.Enqueue(ctx, jobs.TypeBackupUserData, 0, now); err != nil {
			return err
		}
	}
	if anilist {
		if _, err := svc.Enqueue(ctx, jobs.TypeSyncTrackers, 0, now); err != nil {
			return err
		}
	}
	if catalogRefresh {
		if _, err := svc.Enqueue(ctx, jobs.TypeCatalogRefresh, 0, now); err != nil {
			return err
		}
	}
	return nil
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
