package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"time"

	"github.com/brogergvhs/kaodoku/internal/database"
)

// startedAt approximates process start, for the uptime metric.
var startedAt = time.Now()

// stripPageThreshold classifies a chapter as a long-strip (webtoon/manhwa)
// rather than book-paged manga when it has at most this many image "pages".
// Long strips are a handful of tall images; book pages run to the dozens.
// Per-page reading time is only ever compared within a cohort, never across.
const stripPageThreshold = 8

// Gaps longer than pageDwellCap don't count toward active reading time (the
// reader looked away); gaps beyond sessionGap begin a new reading session.
const (
	pageDwellCap = 3 * time.Minute
	sessionGap   = 30 * time.Minute
	heatmapDays  = 182
)

// genuineJoin restricts chapter_read_pages to chapters read
// in-app page-by-page (manual = 0), excluding bulk/tracker marks that carry no
// real timing. Every time-based metric joins through it.
const genuineJoin = `JOIN chapter_read_progress crp ON crp.user_id = cp.user_id AND crp.chapter_id = cp.chapter_id AND crp.manual = 0`

// DayCount is one day's tally for line charts and heatmaps.
type DayCount struct {
	Day   string // YYYY-MM-DD
	Count int64
}

// NamedCount is a labelled tally with its share of the whole.
type NamedCount struct {
	Name  string
	Count int64
	Pct   float64
}

// ReadingTimeStats keeps reading time format-aware so book-page dwell and
// long-strip dwell are never averaged together.
type ReadingTimeStats struct {
	ActiveMinutes     int64
	MinutesPerChapter float64
	PagedSecPerPage   float64
	StripSecPerImage  float64
	PagedChapters     int64
	StripChapters     int64
}

// TitleProgress is one title's read state for the recent-activity table.
type TitleProgress struct {
	TitleID      int64
	Title        string
	ReadChapters int64
	Total        int64
	LastReadAt   string
	Completed    bool
}

// PersonalMetrics is one user's reading dashboard.
type PersonalMetrics struct {
	Days                      int
	ChaptersRead              int64 // window, genuinely read (page-by-page)
	PagesRead                 int64 // window, genuine
	ChaptersActuallyReadTotal int64 // all-time, read in-app page-by-page
	ChaptersMarkedTotal       int64 // all-time, bulk/tracker-marked (read elsewhere)
	PagesReadTotal            int64 // all-time, genuine
	VolumesReadTotal          int64
	TitlesCompleted           int64
	TitlesInProgress          int64
	Backlog                   int64
	StreakDays                int
	LongestStreak             int
	ActiveDays                int
	PerDay                    []DayCount
	Heatmap                   []DayCount
	HourHist                  [24]int64
	WeekdayHist               [7]int64
	TopGenres                 []NamedCount
	TopTags                   []NamedCount
	TopAuthors                []NamedCount
	ScoreBuckets              []NamedCount
	FormatSplit               []NamedCount
	ReadingTime               ReadingTimeStats
	Recent                    []TitleProgress
}

// PersonalMetrics computes a user's reading dashboard over the last `days`.
// allowAdult is the VIEWER's guard: when false, adult titles are kept out of the
// taste breakdowns and recent-titles table (matters when an admin views another
// user's metrics).
func (s *JobService) PersonalMetrics(ctx context.Context, userID int64, days int, allowAdult bool) (PersonalMetrics, error) {
	if days <= 0 {
		days = 30
	}
	m := PersonalMetrics{Days: days}
	since := database.FormatTime(time.Now().AddDate(0, 0, -days))

	// "Genuine" = pages/chapters read in-app page-by-page (manual = 0). Bulk and
	// tracker marks (manual = 1) are counted separately and excluded from page
	// counts and every time-based metric, since they carry no real timing.
	m.PagesReadTotal = s.count(ctx, `SELECT COUNT(*) FROM chapter_read_pages cp `+genuineJoin+` WHERE cp.user_id = ?`, userID)
	m.PagesRead = s.count(ctx, `SELECT COUNT(*) FROM chapter_read_pages cp `+genuineJoin+` WHERE cp.user_id = ? AND cp.read_at >= ?`, userID, since)
	m.ChaptersRead = s.count(ctx, `SELECT COUNT(DISTINCT cp.chapter_id) FROM chapter_read_pages cp `+genuineJoin+` WHERE cp.user_id = ? AND cp.read_at >= ?`, userID, since)
	m.ChaptersActuallyReadTotal = s.count(ctx, `SELECT COUNT(*) FROM chapter_read_progress WHERE user_id = ? AND completed = 1 AND manual = 0`, userID)
	m.ChaptersMarkedTotal = s.count(ctx, `SELECT COUNT(*) FROM chapter_read_progress WHERE user_id = ? AND completed = 1 AND manual = 1`, userID)
	m.VolumesReadTotal = s.count(ctx, `SELECT COUNT(*) FROM volume_read_progress WHERE user_id = ? AND completed = 1`, userID)
	m.Backlog = s.count(ctx, `
		SELECT COUNT(*) FROM chapters c
		JOIN downloads d ON d.chapter_id = c.id AND d.status = 'completed'
		LEFT JOIN chapter_read_progress p ON p.chapter_id = c.id AND p.user_id = ? AND p.completed = 1
		WHERE p.chapter_id IS NULL`, userID)

	if err := s.readingActivity(ctx, userID, days, &m); err != nil {
		return m, err
	}
	if err := s.readingTime(ctx, userID, since, &m); err != nil {
		return m, err
	}
	if err := s.readingTaste(ctx, userID, allowAdult, &m); err != nil {
		return m, err
	}
	if err := s.titleProgress(ctx, userID, allowAdult, &m); err != nil {
		return m, err
	}
	return m, nil
}

func (s *JobService) count(ctx context.Context, q string, args ...any) int64 {
	var n int64
	_ = s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n
}

// dayCounts runs a "date(...) , COUNT(*)" query and returns a day->count map.
func (s *JobService) dayCounts(ctx context.Context, q string, args ...any) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var day string
		var n int64
		if rows.Scan(&day, &n) == nil {
			out[day] = n
		}
	}
	return out, rows.Err()
}

func (s *JobService) readingActivity(ctx context.Context, userID int64, days int, m *PersonalMetrics) error {
	span := heatmapDays
	if days > span {
		span = days
	}
	since := database.FormatTime(time.Now().AddDate(0, 0, -span))
	byDay, err := s.dayCounts(ctx,
		`SELECT date(cp.read_at, 'localtime') d, COUNT(*) FROM chapter_read_pages cp `+genuineJoin+`
		 WHERE cp.user_id = ? AND cp.read_at >= ? GROUP BY d`, userID, since)
	if err != nil {
		return err
	}
	today := time.Now()
	fill := func(n int) []DayCount {
		out := make([]DayCount, n)
		for i := 0; i < n; i++ {
			d := today.AddDate(0, 0, -(n - 1 - i)).Format("2006-01-02")
			out[i] = DayCount{Day: d, Count: byDay[d]}
		}
		return out
	}
	m.PerDay = fill(days)
	m.Heatmap = fill(heatmapDays)
	for _, dc := range m.PerDay {
		if dc.Count > 0 {
			m.ActiveDays++
		}
	}

	hours, err := s.dayCounts(ctx,
		`SELECT strftime('%H', cp.read_at, 'localtime') h, COUNT(*) FROM chapter_read_pages cp `+genuineJoin+`
		 WHERE cp.user_id = ? AND cp.read_at >= ? GROUP BY h`, userID, since)
	if err != nil {
		return err
	}
	for h, n := range hours {
		if t, e := time.Parse("15", h); e == nil {
			m.HourHist[t.Hour()] = n
		}
	}
	wdays, err := s.dayCounts(ctx,
		`SELECT strftime('%w', cp.read_at, 'localtime') w, COUNT(*) FROM chapter_read_pages cp `+genuineJoin+`
		 WHERE cp.user_id = ? AND cp.read_at >= ? GROUP BY w`, userID, since)
	if err != nil {
		return err
	}
	for w, n := range wdays {
		if i := int(w[0] - '0'); w != "" && i >= 0 && i < 7 {
			m.WeekdayHist[i] = n
		}
	}

	m.StreakDays, m.LongestStreak = s.streaks(ctx, userID)
	return nil
}

// streaks returns the current and longest consecutive-day reading streaks.
func (s *JobService) streaks(ctx context.Context, userID int64) (current, longest int) {
	since := database.FormatTime(time.Now().AddDate(0, 0, -400))
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT date(cp.read_at, 'localtime') FROM chapter_read_pages cp `+genuineJoin+`
		 WHERE cp.user_id = ? AND cp.read_at >= ? ORDER BY 1`, userID, since)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	var days []time.Time
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			if t, e := time.Parse("2006-01-02", d); e == nil {
				days = append(days, t)
			}
		}
	}
	if len(days) == 0 {
		return 0, 0
	}
	run := 1
	longest = 1
	for i := 1; i < len(days); i++ {
		if days[i].Sub(days[i-1]) == 24*time.Hour {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
	}
	// current streak: count back from today/yesterday
	set := map[string]bool{}
	for _, d := range days {
		set[d.Format("2006-01-02")] = true
	}
	cur := time.Now()
	if !set[cur.Format("2006-01-02")] {
		cur = cur.AddDate(0, 0, -1) // today not yet read is fine if yesterday was
	}
	for set[cur.Format("2006-01-02")] {
		current++
		cur = cur.AddDate(0, 0, -1)
	}
	return current, longest
}

func (s *JobService) readingTime(ctx context.Context, userID int64, since string, m *PersonalMetrics) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cp.chapter_id, cp.read_at, COALESCE(crp.total_pages, 0)
		FROM chapter_read_pages cp `+genuineJoin+`
		WHERE cp.user_id = ? AND cp.read_at >= ?
		ORDER BY cp.read_at`, userID, since)
	if err != nil {
		return err
	}
	defer rows.Close()

	var prev time.Time
	var haveprev bool
	var prevCohort int // 0 = unknown page count, 1 = book-paged, 2 = long-strip
	var pagedSec, stripSec float64
	var pagedPages, stripPages int64
	var active time.Duration
	allCh := map[int64]bool{}
	pagedCh, stripCh := map[int64]bool{}, map[int64]bool{}
	for rows.Next() {
		var chID int64
		var at string
		var total int64
		if err := rows.Scan(&chID, &at, &total); err != nil {
			return err
		}
		t, e := database.ParseTime(at)
		if e != nil {
			continue
		}
		// Chapters with an unknown page count (0) stay out of the per-cohort
		// dwell numbers so they can't pollute the manga-vs-webtoon split; they
		// still count toward the format-agnostic totals.
		cohort := 0
		if total > 0 {
			if total <= stripPageThreshold {
				cohort = 2
			} else {
				cohort = 1
			}
		}
		allCh[chID] = true
		switch cohort {
		case 1:
			pagedPages++
			pagedCh[chID] = true
		case 2:
			stripPages++
			stripCh[chID] = true
		}
		if haveprev {
			gap := t.Sub(prev)
			if gap > 0 && gap <= sessionGap {
				if gap > pageDwellCap {
					gap = pageDwellCap
				}
				active += gap
				switch prevCohort {
				case 1:
					pagedSec += gap.Seconds()
				case 2:
					stripSec += gap.Seconds()
				}
			}
		}
		prev, prevCohort, haveprev = t, cohort, true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rt := ReadingTimeStats{
		ActiveMinutes: int64(active.Minutes()),
		PagedChapters: int64(len(pagedCh)),
		StripChapters: int64(len(stripCh)),
	}
	if chs := len(allCh); chs > 0 {
		rt.MinutesPerChapter = active.Minutes() / float64(chs)
	}
	if pagedPages > 0 {
		rt.PagedSecPerPage = pagedSec / float64(pagedPages)
	}
	if stripPages > 0 {
		rt.StripSecPerImage = stripSec / float64(stripPages)
	}
	m.ReadingTime = rt
	m.FormatSplit = pctList([]NamedCount{
		{Name: "Book pages", Count: rt.PagedChapters},
		{Name: "Long strips", Count: rt.StripChapters},
	})
	return nil
}

func (s *JobService) readingTaste(ctx context.Context, userID int64, allowAdult bool, m *PersonalMetrics) error {
	adult := ""
	if !allowAdult {
		adult = " AND COALESCE(cm.is_adult, 0) = 0"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT cm.genres_json, cm.tags_json, cm.authors_json, cm.average_score,
		       COUNT(DISTINCT c.id) AS ch
		FROM titles t
		JOIN chapters c ON c.title_id = t.id
		JOIN chapter_read_progress p ON p.chapter_id = c.id AND p.user_id = ? AND p.completed = 1
		JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE 1=1`+adult+`
		GROUP BY t.id`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	g, tg, au, sb := aggregateTaste(rows)
	if err := rows.Err(); err != nil {
		return err
	}
	m.TopGenres = topN(g, 10)
	m.TopTags = topN(tg, 15)
	m.TopAuthors = topN(au, 10)
	m.ScoreBuckets = sb
	return nil
}

func (s *JobService) titleProgress(ctx context.Context, userID int64, allowAdult bool, m *PersonalMetrics) error {
	adult := ""
	if !allowAdult {
		adult = " AND COALESCE(cm.is_adult, 0) = 0"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.display_title,
		       COUNT(DISTINCT c.id) AS total,
		       COUNT(DISTINCT CASE WHEN p.completed = 1 THEN c.id END) AS rd,
		       COALESCE(MAX(p.last_read_at), '') AS last
		FROM titles t
		JOIN chapters c ON c.title_id = t.id
		LEFT JOIN chapter_read_progress p ON p.chapter_id = c.id AND p.user_id = ?
		LEFT JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE 1=1`+adult+`
		GROUP BY t.id
		HAVING rd > 0
		ORDER BY last DESC`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tp TitleProgress
		if err := rows.Scan(&tp.TitleID, &tp.Title, &tp.Total, &tp.ReadChapters, &tp.LastReadAt); err != nil {
			return err
		}
		tp.Completed = tp.Total > 0 && tp.ReadChapters >= tp.Total
		if tp.Completed {
			m.TitlesCompleted++
		} else {
			m.TitlesInProgress++
		}
		if len(m.Recent) < 12 {
			m.Recent = append(m.Recent, tp)
		}
	}
	return rows.Err()
}

// aggregateTaste accumulates genre/tag/author weights (weighted by chapters
// read) and score bands from a rows set of (genres_json, tags_json,
// authors_json, average_score, weight).
func aggregateTaste(rows *sql.Rows) (genres, tags, authors map[string]int64, scores []NamedCount) {
	genres, tags, authors = map[string]int64{}, map[string]int64{}, map[string]int64{}
	bands := map[string]int64{}
	add := func(m map[string]int64, raw string, w int64) {
		var list []string
		if json.Unmarshal([]byte(raw), &list) != nil {
			return
		}
		for _, v := range list {
			if v != "" {
				m[v] += w
			}
		}
	}
	for rows.Next() {
		var gj, tj, aj string
		var score, w int64
		if rows.Scan(&gj, &tj, &aj, &score, &w) != nil {
			continue
		}
		if w <= 0 {
			w = 1
		}
		add(genres, gj, w)
		add(tags, tj, w)
		add(authors, aj, w)
		if score > 0 {
			bands[scoreBand(score)] += w
		}
	}
	order := []string{"90+", "80–89", "70–79", "60–69", "<60"}
	for _, b := range order {
		if bands[b] > 0 {
			scores = append(scores, NamedCount{Name: b, Count: bands[b]})
		}
	}
	return genres, tags, authors, pctList(scores)
}

func scoreBand(s int64) string {
	switch {
	case s >= 90:
		return "90+"
	case s >= 80:
		return "80–89"
	case s >= 70:
		return "70–79"
	case s >= 60:
		return "60–69"
	default:
		return "<60"
	}
}

// topN sorts a name->count map descending and returns the top n with shares.
func topN(m map[string]int64, n int) []NamedCount {
	out := make([]NamedCount, 0, len(m))
	for k, v := range m {
		out = append(out, NamedCount{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return pctList(out)
}

// pctList fills each entry's share of the list total.
func pctList(list []NamedCount) []NamedCount {
	var total int64
	for _, c := range list {
		total += c.Count
	}
	if total == 0 {
		return list
	}
	for i := range list {
		list[i].Pct = float64(list[i].Count) * 100 / float64(total)
	}
	return list
}

// ---- Overview (instance-wide) ----

// DownloadStats summarises the download pipeline.
type DownloadStats struct {
	Done        int64
	Failed      int64
	Pending     int64
	SuccessRate float64
	AvgSeconds  float64
	TopErrors   []NamedCount
}

// SystemStats reports server runtime health.
type SystemStats struct {
	Version       string
	UptimeSeconds int64
	Goroutines    int
	HeapMB        float64
	SysMB         float64
	NumGC         uint32
	DBSizeBytes   int64
}

// OverviewMetrics is the instance-wide dashboard (gated on stats.view).
type OverviewMetrics struct {
	TotalTitles             int64
	TotalChapters           int64
	TotalDownloadedChapters int64
	TotalVolumes            int64
	TotalSources            int64
	SourcesHealthy          int64
	LibraryBytes            int64
	ActiveUsersDay          int64
	ActiveUsersWeek         int64
	ActiveUsersMonth        int64
	ChaptersPerDay          []DayCount
	TitlesPerDay            []DayCount
	TopRead                 []NamedCount
	TopDownloaded           []NamedCount
	TopFavourited           []NamedCount
	TrendingGenres          []NamedCount
	TrendingTags            []NamedCount
	Downloads               DownloadStats
	Sources                 []NamedCount
	Jobs                    []NamedCount
	TrackerRequests         []TrackerRequests
	System                  SystemStats
	Memory                  MemoryStats
}

// OverviewMetrics computes the instance-wide dashboard. allowAdult controls
// whether adult titles appear in the name leaderboards, mirroring the app's
// per-user content guard.
func (s *JobService) OverviewMetrics(ctx context.Context, allowAdult bool) (OverviewMetrics, error) {
	m := OverviewMetrics{}
	m.TotalTitles = s.count(ctx, `SELECT COUNT(*) FROM titles`)
	m.TotalChapters = s.count(ctx, `SELECT COUNT(*) FROM chapters`)
	m.TotalDownloadedChapters = s.count(ctx, `SELECT COUNT(DISTINCT chapter_id) FROM downloads WHERE status = 'completed'`)
	m.TotalVolumes = s.count(ctx, `SELECT COUNT(*) FROM volumes`)
	m.TotalSources = s.count(ctx, `SELECT COUNT(*) FROM sources`)
	m.SourcesHealthy = s.count(ctx, `SELECT COUNT(*) FROM sources WHERE status = 'healthy'`)
	m.LibraryBytes = s.count(ctx, `SELECT COALESCE(SUM(bytes),0) FROM downloads WHERE status = 'completed'`) +
		s.count(ctx, `SELECT COALESCE(SUM(bytes),0) FROM volumes`)

	day := database.FormatTime(time.Now().AddDate(0, 0, -1))
	week := database.FormatTime(time.Now().AddDate(0, 0, -7))
	month := database.FormatTime(time.Now().AddDate(0, 0, -30))
	m.ActiveUsersDay = s.count(ctx, `SELECT COUNT(DISTINCT user_id) FROM chapter_read_pages WHERE read_at >= ?`, day)
	m.ActiveUsersWeek = s.count(ctx, `SELECT COUNT(DISTINCT user_id) FROM chapter_read_pages WHERE read_at >= ?`, week)
	m.ActiveUsersMonth = s.count(ctx, `SELECT COUNT(DISTINCT user_id) FROM chapter_read_pages WHERE read_at >= ?`, month)

	since90 := database.FormatTime(time.Now().AddDate(0, 0, -90))
	if chapters, err := s.dayCounts(ctx,
		`SELECT date(discovered_at,'localtime') d, COUNT(*) FROM chapters WHERE discovered_at >= ? GROUP BY d`, since90); err == nil {
		m.ChaptersPerDay = fillDays(chapters, 90)
	}
	if titles, err := s.dayCounts(ctx,
		`SELECT date(created_at,'localtime') d, COUNT(*) FROM titles WHERE created_at >= ? GROUP BY d`, since90); err == nil {
		m.TitlesPerDay = fillDays(titles, 90)
	}

	adultFilter := ""
	if !allowAdult {
		adultFilter = " AND COALESCE(cm.is_adult, 0) = 0"
	}
	m.TopRead = s.leaderboard(ctx, `
		SELECT t.display_title, COUNT(*) n
		FROM chapter_read_progress p
		JOIN chapters c ON c.id = p.chapter_id
		JOIN titles t ON t.id = c.title_id
		LEFT JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE p.completed = 1`+adultFilter+`
		GROUP BY t.id ORDER BY n DESC LIMIT 10`)
	m.TopDownloaded = s.leaderboard(ctx, `
		SELECT t.display_title, COUNT(*) n
		FROM downloads d
		JOIN chapters c ON c.id = d.chapter_id
		JOIN titles t ON t.id = c.title_id
		LEFT JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE d.status = 'completed'`+adultFilter+`
		GROUP BY t.id ORDER BY n DESC LIMIT 10`)
	m.TopFavourited = s.leaderboard(ctx, `
		SELECT t.display_title, COUNT(*) n
		FROM user_favourites uf
		JOIN titles t ON t.id = uf.title_id
		LEFT JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE 1=1`+adultFilter+`
		GROUP BY t.id ORDER BY n DESC LIMIT 10`)

	if rows, err := s.db.QueryContext(ctx, `
		SELECT cm.genres_json, cm.tags_json, cm.authors_json, cm.average_score, COUNT(DISTINCT c.id)
		FROM titles t
		JOIN chapters c ON c.title_id = t.id
		JOIN chapter_read_progress p ON p.chapter_id = c.id AND p.completed = 1
		JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE 1=1`+adultFilter+`
		GROUP BY t.id`); err == nil {
		g, tg, _, _ := aggregateTaste(rows)
		rows.Close()
		m.TrendingGenres = topN(g, 10)
		m.TrendingTags = topN(tg, 12)
	}

	m.Downloads = s.downloadStats(ctx)
	m.Sources = s.leaderboard(ctx, `SELECT status, COUNT(*) FROM sources GROUP BY status ORDER BY 2 DESC`)
	m.Jobs = s.leaderboard(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status ORDER BY 2 DESC`)
	m.TrackerRequests = s.TrackerRequestStats(ctx)
	m.System = s.systemStats()
	m.Memory = s.MemoryStats(ctx)
	return m, nil
}

// leaderboard runs a "label, count" query into a NamedCount list with shares.
func (s *JobService) leaderboard(ctx context.Context, q string, args ...any) []NamedCount {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []NamedCount
	for rows.Next() {
		var nc NamedCount
		if rows.Scan(&nc.Name, &nc.Count) == nil {
			out = append(out, nc)
		}
	}
	return pctList(out)
}

func (s *JobService) downloadStats(ctx context.Context) DownloadStats {
	d := DownloadStats{
		Done:    s.count(ctx, `SELECT COUNT(*) FROM downloads WHERE status = 'completed'`),
		Failed:  s.count(ctx, `SELECT COUNT(*) FROM downloads WHERE status = 'failed'`),
		Pending: s.count(ctx, `SELECT COUNT(*) FROM downloads WHERE status = 'started'`),
	}
	if fin := d.Done + d.Failed; fin > 0 {
		d.SuccessRate = float64(d.Done) * 100 / float64(fin)
	}
	var avg sql.NullFloat64
	_ = s.db.QueryRowContext(ctx, `
		SELECT AVG((julianday(completed_at) - julianday(started_at)) * 86400)
		FROM downloads WHERE status = 'completed' AND started_at != '' AND completed_at IS NOT NULL`).Scan(&avg)
	if avg.Valid {
		d.AvgSeconds = avg.Float64
	}
	d.TopErrors = s.leaderboard(ctx, `
		SELECT error, COUNT(*) n FROM downloads
		WHERE status = 'failed' AND error != ''
		GROUP BY error ORDER BY n DESC LIMIT 8`)
	return d
}

// Version is set at build time with -ldflags; plain builds fall back to the version Go stamps from the module.
var Version = "dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); Version == "dev" && ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}

func (s *JobService) systemStats() SystemStats {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	st := SystemStats{
		Version:       Version,
		UptimeSeconds: int64(time.Since(startedAt).Seconds()),
		Goroutines:    runtime.NumGoroutine(),
		HeapMB:        float64(mem.HeapAlloc) / (1 << 20),
		SysMB:         float64(mem.Sys) / (1 << 20),
		NumGC:         mem.NumGC,
	}
	if fi, err := os.Stat(s.dbPath); err == nil {
		st.DBSizeBytes = fi.Size()
	}
	return st
}

// fillDays turns a day->count map into a dense series of the last n days.
func fillDays(byDay map[string]int64, n int) []DayCount {
	today := time.Now()
	out := make([]DayCount, n)
	for i := 0; i < n; i++ {
		d := today.AddDate(0, 0, -(n - 1 - i)).Format("2006-01-02")
		out[i] = DayCount{Day: d, Count: byDay[d]}
	}
	return out
}
