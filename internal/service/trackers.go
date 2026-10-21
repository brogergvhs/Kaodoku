package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
)

type TrackerAccount struct {
	Provider     string
	Label        string
	Name         string
	ExpiresAt    string
	LastError    string
	ExpiringSoon bool // no refresh token and under 30 days of validity left
	TrackerPrefs
}

type TrackerPrefs struct {
	AutoLink, AutoProgressive, Relations, Recommendations bool
}

func (s *JobService) TrackerAccounts(ctx context.Context, userID int64) []TrackerAccount {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, remote_name, expires_at, last_error, refresh_token = '', auto_link, auto_progressive, use_relations, use_recommendations FROM tracker_accounts WHERE user_id = ? ORDER BY created_at, rowid`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []TrackerAccount
	for rows.Next() {
		var a TrackerAccount
		var noRefresh bool
		if rows.Scan(&a.Provider, &a.Name, &a.ExpiresAt, &a.LastError, &noRefresh, &a.AutoLink, &a.AutoProgressive, &a.Relations, &a.Recommendations) != nil {
			continue
		}
		if _, known := s.catalog.providers[a.Provider]; !known {
			continue
		}
		a.Label = s.ProviderLabel(a.Provider)
		if t, err := database.ParseTime(a.ExpiresAt); err == nil && a.ExpiresAt != "" && noRefresh {
			a.ExpiringSoon = time.Until(t) < 30*24*time.Hour
		}
		out = append(out, a)
	}
	return out
}

func (s *JobService) trackerAccount(ctx context.Context, userID int64, provider string) (TrackerAccount, bool) {
	for _, a := range s.TrackerAccounts(ctx, userID) {
		if a.Provider == provider {
			return a, true
		}
	}
	return TrackerAccount{}, false
}

func (s *JobService) SetTrackerPrefs(ctx context.Context, provider string, p TrackerPrefs) error {
	uid := auth.UserID(ctx)
	res, err := s.db.ExecContext(ctx, `UPDATE tracker_accounts SET auto_link = ?, auto_progressive = ?, use_relations = ?, use_recommendations = ? WHERE user_id = ? AND provider = ?`,
		p.AutoLink, p.AutoProgressive, p.Relations, p.Recommendations, uid, provider)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no %s account connected", s.ProviderLabel(provider))
	}
	s.invalidateRecs(uid)
	return nil
}

type UnlinkedTitle struct {
	library.Title
	Suggested bool
}

// UnlinkedTitles lists library titles with no confirmed link to provider, skipping ones marked as not on it.
func (s *JobService) UnlinkedTitles(ctx context.Context, provider string) ([]UnlinkedTitle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, COALESCE(e.provider_id, '') != '' FROM titles t
		JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		LEFT JOIN catalog_external_ids e ON e.catalog_manga_id = cm.id AND e.provider = ?
		WHERE cm.provider != ? AND COALESCE(e.confirmed, 0) = 0 AND COALESCE(e.method, '') != 'user'`, provider, provider)
	if err != nil {
		return nil, err
	}
	suggested := map[int64]bool{}
	var ids []int64
	for rows.Next() {
		var id int64
		var sug bool
		if rows.Scan(&id, &sug) == nil {
			ids, suggested[id] = append(ids, id), sug
		}
	}
	rows.Close()
	titles, err := s.lib.repo.TitlesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]UnlinkedTitle, 0, len(titles))
	for _, t := range titles {
		out = append(out, UnlinkedTitle{Title: t, Suggested: suggested[t.ID]})
	}
	slices.SortFunc(out, func(a, b UnlinkedTitle) int {
		return strings.Compare(strings.ToLower(a.DisplayTitle), strings.ToLower(b.DisplayTitle))
	})
	return out, nil
}

// titleAdded auto-links a new library title for trackers that opted in, then pushes progressive ones.
func (s *JobService) titleAdded(ctx context.Context, userID, titleID int64) {
	ctx = context.WithoutCancel(ctx)
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return
	}
	accounts := s.TrackerAccounts(ctx, userID)
	go func() {
		for _, a := range accounts {
			if p, err := s.Provider(ctx, a.Provider); err == nil && a.AutoLink {
				s.resolveID(ctx, m, p)
			}
		}
		s.pushTrackers(ctx, userID, titleID, false)
	}()
}

func (s *JobService) connectedTrackers(ctx context.Context, userID int64) []string {
	var out []string
	for _, a := range s.TrackerAccounts(ctx, userID) {
		if _, err := s.Provider(ctx, a.Provider); err == nil {
			out = append(out, a.Provider)
		}
	}
	return out
}

func (s *JobService) TrackerApp(ctx context.Context, provider, redirectURI string) catalog.App {
	switch provider {
	case catalog.AniListProvider:
		return catalog.App{ClientID: s.Setting(ctx, SettingAniListClientID, ""), ClientSecret: s.Setting(ctx, SettingAniListClientSecret, ""), RedirectURI: redirectURI}
	case catalog.MALProvider:
		return catalog.App{ClientID: s.Setting(ctx, SettingMALClientID, ""), ClientSecret: s.Setting(ctx, SettingMALClientSecret, ""), RedirectURI: redirectURI}
	}
	return catalog.App{RedirectURI: redirectURI}
}

func (s *JobService) TrackerLogin(ctx context.Context, provider string) string {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return ""
	}
	if _, ok := p.(catalog.PasswordLogin); ok {
		return "password"
	}
	app := s.TrackerApp(ctx, provider, "")
	if app.ClientID == "" || (provider == catalog.AniListProvider && app.ClientSecret == "") {
		return ""
	}
	return "oauth"
}

func (s *JobService) ConnectOAuth(ctx context.Context, userID int64, provider, redirectURI, code, verifier string) error {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return err
	}
	o, ok := p.(catalog.OAuth)
	if !ok || s.TrackerLogin(ctx, provider) != "oauth" {
		return fmt.Errorf("the %s application is not configured (client id/secret in settings)", p.Label())
	}
	tok, err := o.Exchange(ctx, s.TrackerApp(ctx, provider, redirectURI), code, verifier)
	if err != nil {
		return err
	}
	return s.saveAccount(ctx, userID, p, tok)
}

func (s *JobService) LoginTracker(ctx context.Context, userID int64, provider, username, password string) error {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return err
	}
	l, ok := p.(catalog.PasswordLogin)
	if !ok {
		return fmt.Errorf("%s does not sign in with a password", p.Label())
	}
	tok, err := l.Login(ctx, strings.TrimSpace(username), password)
	if err != nil {
		if catalog.IsUnauthorized(err) {
			return fmt.Errorf("%s rejected that username or password", p.Label())
		}
		return err
	}
	return s.saveAccount(ctx, userID, p, tok)
}

func (s *JobService) saveAccount(ctx context.Context, userID int64, p catalog.Provider, tok catalog.Token) error {
	remoteID, name, err := p.Viewer(catalog.WithAccount(ctx, catalog.Account{Token: tok.Access}))
	if err != nil {
		return err
	}
	expires := ""
	if tok.ExpiresIn > 0 {
		expires = database.FormatTime(time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second))
	}
	access, err := s.secrets.Encrypt(tok.Access)
	if err != nil {
		return err
	}
	refresh := ""
	if tok.Refresh != "" {
		if refresh, err = s.secrets.Encrypt(tok.Refresh); err != nil {
			return err
		}
	}
	if _, err = s.db.ExecContext(ctx, `
		INSERT INTO tracker_accounts (user_id, provider, access_token, refresh_token, remote_user_id, remote_name, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, provider) DO UPDATE SET
			access_token = excluded.access_token, refresh_token = excluded.refresh_token,
			remote_user_id = excluded.remote_user_id, remote_name = excluded.remote_name,
			expires_at = excluded.expires_at, last_error = ''
	`, userID, p.Name(), access, refresh, remoteID, name, expires); err != nil {
		return err
	}
	s.invalidateRecs(userID)
	_, err = s.enqueueExact(ctx, jobs.TypeSyncTrackers, JobPayload{UserID: userID, Provider: p.Name()}, time.Now())
	return err
}

func (s *JobService) DisconnectTracker(ctx context.Context, userID int64, provider string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tracker_accounts WHERE user_id = ? AND provider = ?`, userID, provider); err != nil {
		return err
	}
	s.invalidateRecs(userID)
	return nil
}

func (s *JobService) trackerSession(ctx context.Context, userID int64, provider string) (catalog.Provider, context.Context, bool) {
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return nil, ctx, false
	}
	var token, refresh, remote, expires string
	if err := s.db.QueryRowContext(ctx, `SELECT access_token, refresh_token, remote_user_id, expires_at FROM tracker_accounts WHERE user_id = ? AND provider = ?`, userID, provider).Scan(&token, &refresh, &remote, &expires); err != nil {
		return nil, ctx, false
	}
	if token, err = s.secrets.Decrypt(token); err != nil || token == "" {
		log.Printf("%s token decrypt failed for user %d: %v", provider, userID, err)
		return nil, ctx, false
	}
	r, canRefresh := p.(catalog.Refresher)
	if t, err := database.ParseTime(expires); canRefresh && refresh != "" && err == nil && time.Until(t) < 72*time.Hour {
		if plain, err := s.secrets.Decrypt(refresh); err == nil {
			if tok, err := r.Refresh(ctx, s.TrackerApp(ctx, provider, ""), plain); err == nil {
				if tok.Refresh == "" {
					tok.Refresh = plain
				}
				if err := s.storeToken(ctx, userID, provider, tok); err == nil {
					token = tok.Access
				}
			}
		}
	}
	return p, catalog.WithAccount(ctx, catalog.Account{Token: token, UserID: remote}), true
}

func (s *JobService) storeToken(ctx context.Context, userID int64, provider string, tok catalog.Token) error {
	access, err := s.secrets.Encrypt(tok.Access)
	if err != nil {
		return err
	}
	refresh, err := s.secrets.Encrypt(tok.Refresh)
	if err != nil {
		return err
	}
	expires := database.FormatTime(time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second))
	_, err = s.db.ExecContext(ctx, `UPDATE tracker_accounts SET access_token = ?, refresh_token = ?, expires_at = ? WHERE user_id = ? AND provider = ?`, access, refresh, expires, userID, provider)
	return err
}

func (s *JobService) trackerAuthError(ctx context.Context, userID int64, provider string, err error) error {
	if !catalog.IsUnauthorized(err) {
		return err
	}
	label := s.ProviderLabel(provider)
	msg := fmt.Errorf("%s authorization expired; reconnect %s in settings", label, label)
	if derr := s.DisconnectTracker(ctx, userID, provider); derr != nil {
		return errors.Join(msg, derr)
	}
	return msg
}

func (s *JobService) EnqueueTrackerSync(ctx context.Context, userID int64, provider string) error {
	if provider != "" {
		if _, _, ok := s.trackerSession(ctx, userID, provider); !ok {
			return fmt.Errorf("no %s account connected", s.ProviderLabel(provider))
		}
	} else if len(s.connectedTrackers(ctx, userID)) == 0 {
		return fmt.Errorf("no tracker account connected")
	}
	_, err := s.enqueueExact(ctx, jobs.TypeSyncTrackers, JobPayload{UserID: userID, Provider: provider}, time.Now())
	return err
}

func (s *JobService) expandTrackerSync(ctx context.Context, parentID int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, provider FROM tracker_accounts`)
	if err != nil {
		return err
	}
	var payloads []JobPayload
	for rows.Next() {
		var p JobPayload
		if rows.Scan(&p.UserID, &p.Provider) == nil {
			payloads = append(payloads, p)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range payloads {
		_, err := s.enqueueChild(ctx, jobs.TypeSyncTrackers, p, time.Now(), parentID)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// runTrackerSync: remote > local pulls read marks down, local > remote pushes up, missing entries get added.
func (s *JobService) runTrackerSync(ctx context.Context, userID int64, provider string, progress ProgressManager) error {
	user, err := s.auth.GetUser(ctx, userID)
	if err != nil || user == nil {
		user = &auth.User{ID: userID}
	}
	ctx = auth.WithUser(ctx, user)
	if provider == "" {
		var errs []error
		for _, name := range s.connectedTrackers(ctx, userID) {
			errs = append(errs, s.runTrackerSync(ctx, userID, name, progress))
		}
		return errors.Join(errs...)
	}
	p, actx, ok := s.trackerSession(ctx, userID, provider)
	if !ok {
		return nil
	}
	if progress == nil {
		progress = noopProgress{}
	}
	err = s.trackerAuthError(ctx, userID, provider, s.syncTracker(ctx, actx, user, p, progress))
	_, _ = s.db.ExecContext(ctx, `UPDATE tracker_accounts SET last_error = ? WHERE user_id = ? AND provider = ?`, errText(err), userID, provider)
	s.invalidateRecs(userID)
	return err
}

type noopProgress struct{}

func (noopProgress) Register(string) ProgressHandle { return noopProgressHandle{} }
func (noopProgress) Close()                         {}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *JobService) syncTracker(ctx, actx context.Context, user *auth.User, p catalog.Provider, progress ProgressManager) error {
	entries, err := p.ListEntries(actx)
	if err != nil {
		return err
	}
	remote := make(map[string]catalog.Entry, len(entries))
	for _, e := range entries {
		remote[e.Manga.ProviderID] = e
	}
	if a, _ := s.trackerAccount(ctx, user.ID, p.Name()); a.AutoLink {
		s.resolveLibrary(ctx, p, 100)
	}
	tracked, err := s.lib.TitlesByProvider(ctx, p.Name())
	if err != nil {
		return err
	}
	enabled, err := s.progressiveTitles(ctx, user.ID, p.Name())
	if err != nil {
		return err
	}
	maps.DeleteFunc(tracked, func(_ string, id int64) bool { return !enabled(id) })
	trackedIDs := make([]int64, 0, len(tracked))
	for _, id := range tracked {
		trackedIDs = append(trackedIDs, id)
	}
	titles, err := s.lib.repo.TitlesByIDs(ctx, trackedIDs)
	if err != nil {
		return err
	}
	byID := make(map[int64]library.Title, len(titles))
	for _, t := range titles {
		byID[t.ID] = t
	}
	owners, err := s.lib.TitleOwners(ctx)
	if err != nil {
		return err
	}
	fav, canFav := p.(catalog.Favouriter)
	remoteFav := map[string]bool{}
	if canFav {
		ids, err := fav.Favourites(actx)
		canFav = err == nil
		for _, id := range ids {
			remoteFav[id] = true
		}
	}
	localFav, err := s.localFavouriteIDs(ctx, user.ID, p.Name())
	if err != nil {
		return err
	}
	handle := progress.Register(p.Name() + " sync")
	handle.SetTotal(len(tracked))
	defer handle.MarkDone()
	var errs []error
	n := 0
	for pid, titleID := range tracked {
		n++
		handle.Update(n, len(tracked), 0)
		t := byID[titleID]
		if !contentAllowedFor(user, t.IsAdult, t.ContentTags) {
			continue
		}
		if canFav && remoteFav[pid] && !localFav[pid] {
			if err := s.lib.SetFavourite(ctx, user.ID, titleID); err != nil {
				errs = append(errs, err)
			}
		} else if canFav && localFav[pid] && !remoteFav[pid] {
			if err := fav.ToggleFavourite(actx, pid); err != nil {
				errs = append(errs, fmt.Errorf("favourite %s: %w", pid, err))
			}
		}
		e, listed := remote[pid]
		if listed && e.Progress > 0 {
			local, _, err := s.localTrackerState(ctx, user.ID, titleID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if e.Progress > local {
				if _, err := s.lib.MarkChaptersReadThrough(ctx, titleID, e.Progress); err != nil {
					errs = append(errs, fmt.Errorf("pull %s: %w", pid, err))
					continue
				}
			}
		}
		local, err := s.localUpdate(ctx, p, user.ID, titleID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !listed {
			if local.Status == "PLANNING" && !ownsTitle(user.ID, owners[titleID]) {
				continue
			}
			if err := p.SaveEntry(actx, pid, local); err != nil {
				errs = append(errs, fmt.Errorf("add %s: %w", pid, err))
			}
			continue
		}
		if u := entryDiff(local, e, false); !u.Empty() {
			if err := p.SaveEntry(actx, pid, u); err != nil {
				errs = append(errs, fmt.Errorf("push %s: %w", pid, err))
			}
		}
	}
	return errs2err(errs)
}

// localUpdate dates count only reading done in kaodoku and go only to backends that store them.
func (s *JobService) localUpdate(ctx context.Context, p catalog.Provider, userID, titleID int64) (catalog.EntryUpdate, error) {
	progress, status, err := s.localTrackerState(ctx, userID, titleID)
	u := catalog.EntryUpdate{Progress: progress, Status: status}
	if d, ok := p.(catalog.DateTracker); err != nil || !ok || !d.TracksDates() {
		return u, err
	}
	var started, finished sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT MIN(CASE WHEN rp.manual = 0 THEN rp.started_at END),
			MAX(CASE WHEN rp.manual = 0 AND rp.completed = 1 THEN rp.completed_at END)
		FROM chapter_read_progress rp JOIN chapters c ON c.id = rp.chapter_id
		WHERE rp.user_id = ? AND c.title_id = ?`, userID, titleID).Scan(&started, &finished); err != nil {
		return u, err
	}
	if status != "PLANNING" && started.Valid {
		u.Started, _ = database.ParseTime(started.String)
	}
	if status == "COMPLETED" && finished.Valid {
		u.Finished, _ = database.ParseTime(finished.String)
	}
	return u, nil
}

func entryDiff(local catalog.EntryUpdate, remote catalog.Entry, exact bool) catalog.EntryUpdate {
	out := catalog.EntryUpdate{Progress: -1}
	if (exact && local.Progress != remote.Progress) || local.Progress > remote.Progress {
		out.Progress = local.Progress
	}
	if statusRank(local.Status) > statusRank(remote.Status) {
		out.Status = local.Status
	}
	if remote.Started.IsZero() {
		out.Started = local.Started
	}
	if remote.Finished.IsZero() {
		out.Finished = local.Finished
	}
	return out
}

// reconcileEntry only raises remote progress/status and fills empty dates; exact also lowers progress after an explicit unread.
func reconcileEntry(ctx context.Context, p catalog.Provider, id string, local catalog.EntryUpdate, exact bool) error {
	e, found, err := p.GetEntry(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return p.SaveEntry(ctx, id, local)
	}
	if u := entryDiff(local, e, exact); !u.Empty() {
		return p.SaveEntry(ctx, id, u)
	}
	return nil
}

func (s *JobService) PushTrackers(ctx context.Context, userID, titleID int64) {
	s.pushTrackers(ctx, userID, titleID, false)
}

func (s *JobService) PushTrackersExact(ctx context.Context, userID, titleID int64) {
	s.pushTrackers(ctx, userID, titleID, true)
}

func (s *JobService) pushTrackers(ctx context.Context, userID, titleID int64, exact bool) {
	ctx = context.WithoutCancel(ctx)
	names := s.progressiveTrackers(ctx, userID, titleID)
	if len(names) == 0 {
		return
	}
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return
	}
	s.eachTracker(ctx, userID, names, func(actx context.Context, p catalog.Provider) error {
		id := s.confirmedID(actx, m, p.Name())
		if id == "" {
			return nil
		}
		local, err := s.localUpdate(ctx, p, userID, titleID)
		if err != nil {
			return err
		}
		return reconcileEntry(actx, p, id, local, exact)
	})
}

func (s *JobService) eachTracker(ctx context.Context, userID int64, names []string, fn func(context.Context, catalog.Provider) error) {
	go func() {
		for _, name := range names {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			if p, actx, ok := s.trackerSession(pctx, userID, name); ok {
				if err := fn(actx, p); err != nil {
					log.Printf("%s push for user %d: %v", name, userID, s.trackerAuthError(ctx, userID, name, err))
				}
			}
			cancel()
		}
		s.invalidateRecs(userID)
	}()
}

func (s *JobService) dropFromTrackers(ctx context.Context, userID int64, names []string, m catalog.Manga, remove bool) {
	ctx = context.WithoutCancel(ctx)
	s.eachTracker(ctx, userID, names, func(actx context.Context, p catalog.Provider) error {
		id := s.confirmedID(actx, m, p.Name())
		switch {
		case id == "":
			return nil
		case remove:
			return p.DeleteEntry(actx, id)
		default:
			return p.SaveEntry(actx, id, catalog.EntryUpdate{Progress: -1, Status: "DROPPED"})
		}
	})
}

func (s *JobService) pushFavourite(ctx context.Context, userID, titleID int64, want bool) {
	ctx = context.WithoutCancel(ctx)
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return
	}
	s.eachTracker(ctx, userID, s.progressiveTrackers(ctx, userID, titleID), func(actx context.Context, p catalog.Provider) error {
		fav, ok := p.(catalog.Favouriter)
		id := s.confirmedID(actx, m, p.Name())
		if !ok || id == "" {
			return nil
		}
		remote, err := fav.IsFavourite(actx, id)
		if err != nil || remote == want {
			return err
		}
		return fav.ToggleFavourite(actx, id)
	})
}

func (s *JobService) localFavouriteIDs(ctx context.Context, userID int64, provider string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cm.provider_id FROM user_favourites uf
		JOIN titles t ON t.id = uf.title_id JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		WHERE uf.user_id = ? AND cm.provider = ?
		UNION
		SELECT e.provider_id FROM user_favourites uf
		JOIN titles t ON t.id = uf.title_id JOIN catalog_external_ids e ON e.catalog_manga_id = t.catalog_manga_id
		WHERE uf.user_id = ? AND e.provider = ? AND e.confirmed = 1 AND e.provider_id != ''`, userID, provider, userID, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var pid string
		if rows.Scan(&pid) == nil {
			out[pid] = true
		}
	}
	return out, rows.Err()
}

// SyncTrackersTitle is a one-off sync of a title; an empty provider means every connected tracker.
func (s *JobService) SyncTrackersTitle(ctx context.Context, titleID int64, provider string) error {
	userID := auth.UserID(ctx)
	names := s.connectedTrackers(ctx, userID)
	if provider != "" {
		if !slices.Contains(names, provider) {
			return fmt.Errorf("connect %s in settings first", s.ProviderLabel(provider))
		}
		names = []string{provider}
	}
	if len(names) == 0 {
		return fmt.Errorf("no tracker account connected")
	}
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return fmt.Errorf("title has no catalog entry")
	}
	var errs []error
	linked := 0
	for _, name := range names {
		p, actx, ok := s.trackerSession(ctx, userID, name)
		if !ok {
			continue
		}
		id := s.confirmedID(ctx, m, name)
		if id == "" {
			continue
		}
		linked++
		errs = append(errs, s.trackerAuthError(ctx, userID, name, s.syncTitle(ctx, actx, p, id, userID, titleID)))
	}
	if linked == 0 {
		return fmt.Errorf("title is not linked on any connected tracker")
	}
	s.invalidateRecs(userID)
	return errors.Join(errs...)
}

// SetProgressiveSync stores an explicit per-title choice, which overrides the account's auto progressive default.
func (s *JobService) SetProgressiveSync(ctx context.Context, titleID int64, provider string, on bool) error {
	if on {
		if err := s.SyncTrackersTitle(ctx, titleID, provider); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tracker_title_sync (user_id, title_id, provider, enabled) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, title_id, provider) DO UPDATE SET enabled = excluded.enabled`, auth.UserID(ctx), titleID, provider, on)
	return err
}

func (s *JobService) progressiveTrackers(ctx context.Context, userID, titleID int64) []string {
	var out []string
	for _, a := range s.TrackerAccounts(ctx, userID) {
		if !s.providerConfigured(ctx, a.Provider) || !auth.FromContext(ctx).ProviderAllowed(a.Provider) {
			continue
		}
		on := a.AutoProgressive
		_ = s.db.QueryRowContext(ctx, `SELECT enabled FROM tracker_title_sync WHERE user_id = ? AND title_id = ? AND provider = ?`, userID, titleID, a.Provider).Scan(&on)
		if on {
			out = append(out, a.Provider)
		}
	}
	return out
}

func (s *JobService) progressiveTitles(ctx context.Context, userID int64, provider string) (func(int64) bool, error) {
	a, _ := s.trackerAccount(ctx, userID, provider)
	rows, err := s.db.QueryContext(ctx, `SELECT title_id, enabled FROM tracker_title_sync WHERE user_id = ? AND provider = ?`, userID, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	explicit := map[int64]bool{}
	for rows.Next() {
		var id int64
		var on bool
		if err := rows.Scan(&id, &on); err != nil {
			return nil, err
		}
		explicit[id] = on
	}
	return func(id int64) bool {
		if on, ok := explicit[id]; ok {
			return on
		}
		return a.AutoProgressive
	}, rows.Err()
}

func (s *JobService) syncTitle(ctx, actx context.Context, p catalog.Provider, id string, userID, titleID int64) error {
	remote, found, err := p.GetEntry(actx, id)
	if err != nil {
		return err
	}
	if local, _, err := s.localTrackerState(ctx, userID, titleID); err != nil {
		return err
	} else if found && remote.Progress > local {
		if _, err := s.lib.MarkChaptersReadThrough(ctx, titleID, remote.Progress); err != nil {
			return err
		}
	}
	local, err := s.localUpdate(ctx, p, userID, titleID)
	if err != nil {
		return err
	}
	return reconcileEntry(actx, p, id, local, false)
}

func (s *JobService) TrackerSuggestions(ctx context.Context, provider string) []catalog.Manga {
	var lists [][]catalog.Manga
	uid := auth.UserID(ctx)
	for _, name := range s.connectedTrackers(ctx, uid) {
		if provider != "" && name != provider {
			continue
		}
		p, actx, ok := s.trackerSession(ctx, uid, name)
		if !ok {
			continue
		}
		entries, err := p.ListEntries(actx)
		if err != nil {
			continue
		}
		var list []catalog.Manga
		for _, e := range entries {
			if e.Status != "DROPPED" {
				list = append(list, e.Manga)
			}
		}
		lists = append(lists, list)
	}
	return mergeManga(lists, nil, 0)
}

// ProviderSuggestions approximates a backend's own suggestions from community
// recommendations for the user's reading on it; ok is false when not connected.
func (s *JobService) ProviderSuggestions(ctx context.Context, provider string) ([]catalog.Manga, bool) {
	uid := auth.UserID(ctx)
	key := fmt.Sprintf("%d:%s", uid, provider)
	s.recMu.Lock()
	c, cached := s.sugCache[key]
	s.recMu.Unlock()
	if cached && time.Now().Before(c.expires) {
		return c.items, true
	}
	p, actx, ok := s.trackerSession(ctx, uid, provider)
	if !ok {
		return nil, false
	}
	entries, err := p.ListEntries(actx)
	if err != nil {
		return nil, false
	}
	exclude := map[string]bool{}
	for k := range s.LibraryIndex(ctx) {
		exclude[k] = true
	}
	var seeds []string
	for _, e := range entries {
		for _, k := range catalog.Keys(e.Manga) {
			exclude[k] = true
		}
		if e.Status == "CURRENT" || e.Status == "COMPLETED" || e.Status == "REPEATING" {
			seeds = append(seeds, e.Manga.ProviderID)
		}
	}
	const maxSeeds = 8
	if len(seeds) > maxSeeds {
		spread := make([]string, 0, maxSeeds)
		for i := range maxSeeds {
			spread = append(spread, seeds[i*len(seeds)/maxSeeds])
		}
		seeds = spread
	}
	var lists [][]catalog.Manga
	for _, id := range seeds {
		if _, recs, err := s.relatedCached(ctx, p, id, 24); err == nil {
			lists = append(lists, recs)
		}
	}
	items := mergeManga(lists, exclude, 0)
	s.recMu.Lock()
	if s.sugCache == nil {
		s.sugCache = map[string]cachedRecs{}
	}
	s.sugCache[key] = cachedRecs{items: items, expires: time.Now().Add(recommendationTTL)}
	s.recMu.Unlock()
	return items, true
}

func (s *JobService) encryptLegacyTokens(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, provider, access_token FROM tracker_accounts`)
	if err != nil {
		return err
	}
	type row struct {
		id              int64
		provider, token string
	}
	var legacy []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.provider, &r.token) == nil && r.token != "" && !strings.HasPrefix(r.token, encPrefix) {
			legacy = append(legacy, r)
		}
	}
	rows.Close()
	for _, r := range legacy {
		enc, err := s.secrets.Encrypt(r.token)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tracker_accounts SET access_token = ? WHERE user_id = ? AND provider = ?`, enc, r.id, r.provider); err != nil {
			return err
		}
	}
	return nil
}
