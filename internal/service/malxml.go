package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
)

type malList struct {
	XMLName xml.Name `xml:"myanimelist"`
	Info    struct {
		UserName   string `xml:"user_name"`
		ExportType int    `xml:"user_export_type"`
		Total      int    `xml:"user_total_manga"`
		Reading    int    `xml:"user_total_reading"`
		Completed  int    `xml:"user_total_completed"`
		OnHold     int    `xml:"user_total_onhold"`
		Dropped    int    `xml:"user_total_dropped"`
		PlanToRead int    `xml:"user_total_plantoread"`
	} `xml:"myinfo"`
	Manga []malEntry `xml:"manga"`
}

type malEntry struct {
	ID           string `xml:"manga_mangadb_id"`
	Title        string `xml:"manga_title"`
	Volumes      int    `xml:"manga_volumes"`
	Chapters     int    `xml:"manga_chapters"`
	ReadVolumes  int    `xml:"my_read_volumes"`
	ReadChapters int    `xml:"my_read_chapters"`
	Start        string `xml:"my_start_date"`
	Finish       string `xml:"my_finish_date"`
	Score        int    `xml:"my_score"`
	Status       string `xml:"my_status"`
	TimesRead    int    `xml:"my_times_read"`
	Rereading    string `xml:"my_rereading"`
	Update       int    `xml:"update_on_import"`
}

func malDate(t time.Time) string {
	if t.IsZero() {
		return "0000-00-00"
	}
	return t.Format(time.DateOnly)
}

// ExportMALXML writes the user's library as a MyAnimeList manga export and counts the titles left out for lacking a MyAnimeList id.
func (s *JobService) ExportMALXML(ctx context.Context, w io.Writer) (exported, skipped int, err error) {
	user, uid := auth.FromContext(ctx), auth.UserID(ctx)
	byMAL, err := s.lib.TitlesByProvider(ctx, catalog.MALProvider)
	if err != nil {
		return 0, 0, err
	}
	malOf := map[int64]string{}
	for id, tid := range byMAL {
		malOf[tid] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM titles WHERE catalog_manga_id IS NOT NULL`)
	if err != nil {
		return 0, 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	titles, err := s.lib.repo.TitlesByIDs(ctx, ids)
	if err != nil {
		return 0, 0, err
	}
	owners, err := s.lib.TitleOwners(ctx)
	if err != nil {
		return 0, 0, err
	}
	var doc malList
	doc.Info.UserName, doc.Info.ExportType = user.Username, 2
	for _, t := range titles {
		u, err := s.localUpdate(ctx, s.catalog.providers[catalog.MALProvider], uid, t.ID)
		if err != nil || !contentAllowedFor(user, t.IsAdult, t.ContentTags) || u.Status == "PLANNING" && !ownsTitle(uid, owners[t.ID]) {
			continue
		}
		if malOf[t.ID] == "" {
			skipped++
			continue
		}
		score, _ := s.userRating(ctx, uid, t.ID)
		e := malEntry{ID: malOf[t.ID], Title: t.DisplayTitle, ReadChapters: u.Progress, Score: score, Start: malDate(u.Started), Finish: malDate(u.Finished), Rereading: "NO", Update: 1}
		switch u.Status {
		case "COMPLETED":
			e.Status, doc.Info.Completed = "Completed", doc.Info.Completed+1
		case "CURRENT":
			e.Status, doc.Info.Reading = "Reading", doc.Info.Reading+1
		default:
			e.Status, doc.Info.PlanToRead = "Plan to Read", doc.Info.PlanToRead+1
		}
		doc.Manga = append(doc.Manga, e)
	}
	doc.Info.Total = len(doc.Manga)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return 0, 0, err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return len(doc.Manga), skipped, enc.Encode(doc)
}

type ListEntry struct {
	MALID    string `json:"mal_id"`
	Chapters int    `json:"chapters,omitempty"`
	Score    int    `json:"score,omitempty"`
}

type ListImport struct {
	Entries    []ListEntry `json:"entries,omitempty"`
	Progress   bool        `json:"progress,omitempty"`
	Ratings    bool        `json:"ratings,omitempty"`
	Overwrite  bool        `json:"overwrite,omitempty"`
	AddMissing bool        `json:"-"`
}

type ListImportResult struct {
	Applied                          bool
	InLibrary, NotInLibrary, Ignored int
	Raised, Rated, Queued            int
}

// ImportMALXML reads a MyAnimeList manga export. Without apply it only counts; with it, it raises progress,
// sets ratings and queues titles missing from the library.
func (s *JobService) ImportMALXML(ctx context.Context, r io.Reader, opts ListImport, apply bool) (ListImportResult, error) {
	var doc malList
	if err := xml.NewDecoder(io.LimitReader(r, 10<<20)).Decode(&doc); err != nil || doc.Info.ExportType != 2 {
		return ListImportResult{}, fmt.Errorf("not a MyAnimeList manga export")
	}
	user, uid := auth.FromContext(ctx), auth.UserID(ctx)
	byMAL, err := s.lib.TitlesByProvider(ctx, catalog.MALProvider)
	if err != nil {
		return ListImportResult{}, err
	}
	ids := make([]int64, 0, len(byMAL))
	for _, id := range byMAL {
		ids = append(ids, id)
	}
	titles, err := s.lib.repo.TitlesByIDs(ctx, ids)
	if err != nil {
		return ListImportResult{}, err
	}
	byID := map[int64]library.Title{}
	for _, t := range titles {
		byID[t.ID] = t
	}
	res := ListImportResult{Applied: apply}
	var missing []ListEntry
	for _, m := range doc.Manga {
		if m.Status == "Dropped" || m.Status == "On-Hold" {
			res.Ignored++
		}
		e := ListEntry{MALID: strings.TrimSpace(m.ID), Chapters: m.ReadChapters, Score: m.Score}
		tid, ok := byMAL[e.MALID]
		switch {
		case !ok:
			res.NotInLibrary++
			missing = append(missing, e)
		case contentAllowedFor(user, byID[tid].IsAdult, byID[tid].ContentTags):
			res.InLibrary++
			if apply {
				raised, rated := s.applyListEntry(ctx, uid, tid, e, opts)
				res.Raised, res.Rated = res.Raised+raised, res.Rated+rated
			}
		}
	}
	if apply && opts.AddMissing && len(missing) > 0 {
		opts.Entries = missing
		if _, err := s.enqueueExact(ctx, jobs.TypeImportList, JobPayload{UserID: uid, Import: &opts}, time.Now()); err != nil {
			return res, err
		}
		res.Queued = len(missing)
	}
	return res, nil
}

func (s *JobService) applyListEntry(ctx context.Context, uid, titleID int64, e ListEntry, opts ListImport) (raised, rated int) {
	if local, _, err := s.localTrackerState(ctx, uid, titleID); opts.Progress && err == nil && e.Chapters > local {
		if n, err := s.lib.MarkChaptersReadThrough(ctx, titleID, e.Chapters); err == nil && n > 0 {
			raised = 1
			s.pushTrackers(ctx, uid, titleID, false)
		}
	}
	if local, set := s.userRating(ctx, uid, titleID); opts.Ratings && e.Score > 0 && local != e.Score && (!set || local == 0 || opts.Overwrite) {
		if s.SetTitleRating(ctx, titleID, e.Score) == nil {
			rated = 1
		}
	}
	return raised, rated
}

// runListImport adds the imported titles that weren't in the library, then applies their entries.
func (s *JobService) runListImport(ctx context.Context, userID int64, opts *ListImport, progress ProgressManager) error {
	user, err := s.auth.GetUser(ctx, userID)
	if err != nil || user == nil || opts == nil {
		return fmt.Errorf("list import has no user")
	}
	ctx = catalog.WithReason(auth.WithUser(ctx, user), "Import")
	handle := progress.Register("list import")
	defer handle.MarkDone()
	var errs []error
	for i, e := range opts.Entries {
		handle.Update(i+1, len(opts.Entries), 0)
		provider, id := catalog.MALProvider, e.MALID
		if !s.providerEnabled(ctx, provider) {
			found := false
			if r, ok := s.catalog.providers[catalog.AniListProvider].(catalog.Resolver); ok {
				id, found, err = r.Resolve(ctx, catalog.MALProvider, e.MALID)
			}
			if !found || err != nil {
				errs = append(errs, fmt.Errorf("MyAnimeList %s: not found on AniList", e.MALID))
				continue
			}
			provider = catalog.AniListProvider
		}
		title, err := s.AddCatalogTitle(ctx, provider, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("MyAnimeList %s: %w", e.MALID, err))
			continue
		}
		s.applyListEntry(ctx, userID, title.ID, e, *opts)
	}
	return errs2err(errs)
}
