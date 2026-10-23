package service

import (
	"context"
	"strconv"
	"time"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/mangabaka"
	"github.com/brogergvhs/kaodoku/internal/util"
)

const (
	SettingMangaBakaEnabled    = "mangabaka.enabled"
	SettingMangaBakaEvery      = "mangabaka.refresh_every"
	SettingMangaBakaVariant    = "mangabaka.variant"
	SettingMangaBakaAggressive = "mangabaka.aggressive"

	mangaBakaName = "mangabaka"
)

var mangaBakaClient, _ = util.NewHTTPClient(util.HTTPClientOptions{UserAgent: "Kaodoku (+https://github.com/brogergvhs/kaodoku)"})

func (s *JobService) boolSetting(ctx context.Context, key string) bool {
	on, _ := strconv.ParseBool(s.Setting(ctx, key, SettingDefault(key)))
	return on
}

func (s *JobService) MangaBakaEnabled(ctx context.Context) bool {
	return s.boolSetting(ctx, SettingMangaBakaEnabled)
}

// mbOn: MangaBaka is turned on and its copy is loaded.
func (s *JobService) mbOn(ctx context.Context) bool { return s.MangaBakaEnabled(ctx) && s.mb.Ready() }

// mbAggressive: shared data comes only from MangaBaka and what trackers already sent.
func (s *JobService) mbAggressive(ctx context.Context) bool {
	return s.mbOn(ctx) && s.boolSetting(ctx, SettingMangaBakaAggressive)
}

// MangaBakaEvery is how often to check for a new dump; 0 while MangaBaka is off.
func (s *JobService) MangaBakaEvery(ctx context.Context) time.Duration {
	if !s.MangaBakaEnabled(ctx) {
		return 0
	}
	d, _ := time.ParseDuration(s.Setting(ctx, SettingMangaBakaEvery, SettingDefault(SettingMangaBakaEvery)))
	return d
}

func (s *JobService) MangaBakaReady() bool { return s.mb.Ready() }

func (s *JobService) mangaBakaVariant(ctx context.Context) string {
	return s.Setting(ctx, SettingMangaBakaVariant, SettingDefault(SettingMangaBakaVariant))
}

// MangaBakaUnchanged reports whether the published dump is the one already imported.
func (s *JobService) MangaBakaUnchanged(ctx context.Context) (bool, error) {
	v := s.mangaBakaVariant(ctx)
	sum, err := mangabaka.RemoteSHA1(ctx, mangaBakaClient, mangabaka.DumpURL(v))
	m := s.mb.Meta(ctx)
	return err == nil && sum == m.SHA1 && v == m.Variant, err
}

func (s *JobService) RefreshMangaBaka(ctx context.Context, force bool) error {
	_, err := s.enqueueExact(ctx, jobs.TypeMangaBakaRefresh, JobPayload{Force: force}, time.Now())
	return err
}

func (s *JobService) runMangaBakaRefresh(ctx context.Context, force bool, progress ProgressManager) error {
	if !s.MangaBakaEnabled(ctx) {
		return nil
	}
	handle := progress.Register("mangabaka")
	defer handle.MarkDone()
	v := s.mangaBakaVariant(ctx)
	changed, err := s.mb.Refresh(ctx, mangaBakaClient, mangabaka.DumpURL(v), v, force, func(n int) { handle.Update(n, 0, 0) })
	_ = s.catalog.saveCalls(ctx, mangaBakaName, catalog.Call{Kind: "Database", Reason: "Database refresh"}, 1)
	if err != nil || !changed {
		return err
	}
	return nil
}

type MangaBakaStatus struct {
	mangabaka.Meta
	Next      time.Time
	LastError string
}

func (s *JobService) MangaBakaStatus(ctx context.Context) MangaBakaStatus {
	st := MangaBakaStatus{Meta: s.mb.Meta(ctx)}
	if every := s.MangaBakaEvery(ctx); every > 0 {
		if last := s.LastJobTime(ctx, jobs.TypeMangaBakaRefresh); !last.IsZero() {
			st.Next = last.Add(every)
		}
	}
	var status string
	_ = s.db.QueryRowContext(ctx, `SELECT status, last_error FROM jobs WHERE type = ? ORDER BY id DESC LIMIT 1`, jobs.TypeMangaBakaRefresh).Scan(&status, &st.LastError)
	if status != "failed" && status != "dead" {
		st.LastError = ""
	}
	return st
}
