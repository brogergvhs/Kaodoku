// Package jobs stores background jobs.
package jobs

import "time"

const (
	TypeRefreshTitle     = "refresh_title"
	TypeScanDownloads    = "scan_downloads"
	TypeDownloadMissing  = "download_missing"
	TypeSyncTrackers     = "sync_trackers"
	TypeCatalogRefresh   = "catalog_refresh"
	TypeAttachVolumes    = "attach_volumes"
	TypeVerifySource     = "verify_source"
	TypeMatchSources     = "match_sources"
	TypeImportList       = "import_list"
	TypeBackupUserData   = "backup_user_data"
	TypeDetectCrops      = "detect_crops"
	TypeMangaBakaRefresh = "mangabaka_refresh"
)

type Job struct {
	ID        int64
	Type      string
	Status    string
	Payload   string
	RunAfter  time.Time
	Attempts  int
	LastError string
	ParentID  int64 // 0 = top-level; otherwise the global job that spawned it
	CreatedAt time.Time
	UpdatedAt time.Time
}
