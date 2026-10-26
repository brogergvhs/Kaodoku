package service

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
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
	Position     int  // rating precedence among trackers ratings come from
	TrackerPrefs
}

// TrackerPrefs RatingSync is "" to follow progressive sync, "on" or "off"; Source is "kaodoku" or "tracker",
// the side progress, favourites and ratings flow from; Exclusive trusts that only kaodoku writes to the tracker.
type TrackerPrefs struct {
	AutoLink        bool   `json:"auto_link"`
	AutoProgressive bool   `json:"auto_progressive"`
	Relations       bool   `json:"relations"`
	Recommendations bool   `json:"recommendations"`
	RatingSync      string `json:"rating_sync"`
	Source          string `json:"source"`
	Exclusive       bool   `json:"exclusive"`
}

const (
	SourceKaodoku = "kaodoku"
	SourceTracker = "tracker"
)

// ratingFor is the source of a title's rating on this tracker, or "" when ratings do not sync.
func (p TrackerPrefs) ratingFor(progressive bool) string {
	if p.RatingSync == "on" || p.RatingSync == "" && progressive {
		return p.Source
	}
	return ""
}

func (s *JobService) TrackerAccounts(ctx context.Context, userID int64) []TrackerAccount {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, remote_name, expires_at, last_error, refresh_token = '', auto_link, auto_progressive, use_relations, use_recommendations, rating_sync, source, position, exclusive FROM tracker_accounts WHERE user_id = ? ORDER BY created_at, rowid`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []TrackerAccount
	for rows.Next() {
		var a TrackerAccount
		var noRefresh bool
		if rows.Scan(&a.Provider, &a.Name, &a.ExpiresAt, &a.LastError, &noRefresh, &a.AutoLink, &a.AutoProgressive, &a.Relations, &a.Recommendations, &a.RatingSync, &a.Source, &a.Position, &a.Exclusive) != nil {
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
	p.Source = cmp.Or(p.Source, SourceKaodoku)
	if !slices.Contains([]string{"", "on", "off"}, p.RatingSync) || !slices.Contains([]string{SourceKaodoku, SourceTracker}, p.Source) {
		return fmt.Errorf("unknown rating sync setting")
	}
	uid := auth.UserID(ctx)
	prev, _ := s.trackerAccount(ctx, uid, provider)
	res, err := s.db.ExecContext(ctx, `UPDATE tracker_accounts SET auto_link = ?, auto_progressive = ?, use_relations = ?, use_recommendations = ?, rating_sync = ?, source = ?, exclusive = ? WHERE user_id = ? AND provider = ?`,
		p.AutoLink, p.AutoProgressive, p.Relations, p.Recommendations, p.RatingSync, p.Source, p.Exclusive, uid, provider)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no %s account connected", s.ProviderLabel(provider))
	}
	if p.Exclusive && !prev.Exclusive {
		if _, err := s.db.ExecContext(ctx, `UPDATE tracker_synced SET remote = '' WHERE user_id = ? AND provider = ?`, uid, provider); err != nil {
			return err
		}
	}
	s.invalidateRecs(uid)
	return nil
}

// ReorderTrackers stores the user's rating precedence, first wins.
func (s *JobService) ReorderTrackers(ctx context.Context, providers []string) error {
	for pos, provider := range providers {
		if _, err := s.db.ExecContext(ctx, `UPDATE tracker_accounts SET position = ? WHERE user_id = ? AND provider = ?`, pos, auth.UserID(ctx), provider); err != nil {
			return err
		}
	}
	return nil
}

type UnlinkedTitle struct {
	library.Title
	Suggested        bool
	Provider, Format string
	Year             int
}

// UnlinkedTitles lists library titles with no confirmed link to provider, skipping ones marked as not on it.
func (s *JobService) UnlinkedTitles(ctx context.Context, provider string) ([]UnlinkedTitle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, COALESCE(e.provider_id, '') != '', cm.provider, cm.format, cm.year FROM titles t
		JOIN catalog_manga cm ON cm.id = t.catalog_manga_id
		LEFT JOIN catalog_external_ids e ON e.catalog_manga_id = cm.id AND e.provider = ?
		WHERE cm.provider != ? AND COALESCE(e.confirmed, 0) = 0 AND COALESCE(e.method, '') != 'user'`, provider, provider)
	if err != nil {
		return nil, err
	}
	found := map[int64]UnlinkedTitle{}
	var ids []int64
	for rows.Next() {
		var id int64
		var u UnlinkedTitle
		if rows.Scan(&id, &u.Suggested, &u.Provider, &u.Format, &u.Year) == nil {
			ids, found[id] = append(ids, id), u
		}
	}
	rows.Close()
	titles, err := s.lib.repo.TitlesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]UnlinkedTitle, 0, len(titles))
	for _, t := range titles {
		u := found[t.ID]
		u.Title = t
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b UnlinkedTitle) int {
		return strings.Compare(strings.ToLower(a.DisplayTitle), strings.ToLower(b.DisplayTitle))
	})
	return out, nil
}

// titleAdded auto-links a new library title for trackers that opted in, then pushes progressive ones.
func (s *JobService) titleAdded(ctx context.Context, userID, titleID int64) {
	ctx = catalog.WithReason(ctx, "Title added")
	ctx = context.WithoutCancel(ctx)
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return
	}
	accounts := s.TrackerAccounts(ctx, userID)
	go func() {
		s.mbLink(ctx, []catalog.Manga{m})
		for _, a := range accounts {
			if p, err := s.Provider(ctx, a.Provider); err == nil && a.AutoLink {
				s.resolveID(ctx, m, p)
			}
		}
		s.pushTitle(ctx, userID, titleID, true, false)
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
	if _, ok := p.(catalog.TokenLogin); ok {
		return "token"
	}
	app := s.TrackerApp(ctx, provider, "")
	if app.ClientID == "" || (provider == catalog.AniListProvider && app.ClientSecret == "") {
		return ""
	}
	return "oauth"
}

func (s *JobService) ConnectOAuth(ctx context.Context, userID int64, provider, redirectURI, code, verifier string) error {
	ctx = catalog.WithReason(ctx, "Sign-in")
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
	ctx = catalog.WithReason(ctx, "Sign-in")
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return err
	}
	if _, ok := p.(catalog.TokenLogin); ok {
		err := s.saveAccount(ctx, userID, p, catalog.Token{Access: strings.TrimSpace(password)})
		if catalog.IsUnauthorized(err) {
			return fmt.Errorf("%s rejected that access token", p.Label())
		}
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
	if err := s.forgetTrackerState(ctx, userID, p.Name()); err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `
		INSERT INTO tracker_accounts (user_id, provider, access_token, refresh_token, remote_user_id, remote_name, expires_at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM tracker_accounts WHERE user_id = ?))
		ON CONFLICT(user_id, provider) DO UPDATE SET
			access_token = excluded.access_token, refresh_token = excluded.refresh_token,
			remote_user_id = excluded.remote_user_id, remote_name = excluded.remote_name,
			expires_at = excluded.expires_at, last_error = '', list_json = '', list_fetched_at = ''
	`, userID, p.Name(), access, refresh, remoteID, name, expires, userID); err != nil {
		return err
	}
	s.forgetList(userID, p.Name())
	s.invalidateRecs(userID)
	_, err = s.enqueueExact(ctx, jobs.TypeSyncTrackers, JobPayload{UserID: userID, Provider: p.Name()}, time.Now())
	return err
}

func (s *JobService) DisconnectTracker(ctx context.Context, userID int64, provider string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tracker_accounts WHERE user_id = ? AND provider = ?`, userID, provider); err != nil {
		return err
	}
	if err := s.forgetTrackerState(ctx, userID, provider); err != nil {
		return err
	}
	s.forgetList(userID, provider)
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
	expires := ""
	if tok.ExpiresIn > 0 {
		expires = database.FormatTime(time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second))
	}
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
	ctx = catalog.WithReason(ctx, "Tracker sync")
	if provider != "" {
		if _, _, ok := s.trackerSession(ctx, userID, provider); !ok {
			return fmt.Errorf("no %s account connected", s.ProviderLabel(provider))
		}
	} else if len(s.connectedTrackers(ctx, userID)) == 0 {
		return fmt.Errorf("no tracker account connected")
	}
	_, err := s.enqueueExact(ctx, jobs.TypeSyncTrackers, JobPayload{UserID: userID, Provider: provider, Force: true}, time.Now())
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
// Unless full, an account kaodoku leads only syncs titles changed since their last sync.
func (s *JobService) runTrackerSync(ctx context.Context, userID int64, provider string, full bool, progress ProgressManager) error {
	ctx = catalog.WithReason(ctx, "Tracker sync")
	user, err := s.auth.GetUser(ctx, userID)
	if err != nil || user == nil {
		user = &auth.User{ID: userID}
	}
	ctx = auth.WithUser(ctx, user)
	if provider == "" {
		var errs []error
		for _, name := range s.connectedTrackers(ctx, userID) {
			errs = append(errs, s.runTrackerSync(ctx, userID, name, full, progress))
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
	err = s.trackerAuthError(ctx, userID, provider, s.syncTracker(ctx, actx, user, p, full, progress))
	_, _ = s.db.ExecContext(ctx, `UPDATE tracker_accounts SET last_error = ? WHERE user_id = ? AND provider = ?`, errText(err), userID, provider)
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

func (s *JobService) syncTracker(ctx, actx context.Context, user *auth.User, p catalog.Provider, full bool, progress ProgressManager) error {
	a, _ := s.trackerAccount(ctx, user.ID, p.Name())
	if a.AutoLink {
		s.resolveLibrary(ctx, p, 100)
	}
	tracked, err := s.lib.TitlesByProvider(ctx, p.Name())
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(tracked))
	for _, id := range tracked {
		ids = append(ids, id)
	}
	v, err := s.syncView(ctx, user, p.Name(), ids)
	if err != nil {
		return err
	}
	states, saved := map[int64][3]string{}, map[int64]syncedRow{}
	if a.Source == SourceKaodoku {
		saved = s.syncedRows(ctx, user.ID, p.Name(), 0)
		for pid, titleID := range tracked {
			states[titleID] = s.syncParts(ctx, v, p, pid, titleID)
			if !full && (saved[titleID].parts == states[titleID] || s.pushPending(user.ID, titleID)) {
				delete(tracked, pid)
			}
		}
		if !full && len(tracked) == 0 {
			return nil
		}
	}
	defer s.invalidateRecs(user.ID)
	lookup := a.Source == SourceKaodoku && !full && len(tracked) <= maxEntryLookups
	remote := map[string]catalog.Entry{}
	if lookup {
		for pid, titleID := range tracked {
			e, found, err := remoteEntry(actx, p, a.Exclusive, saved[titleID].remote, pid)
			if err != nil {
				return err
			}
			if found {
				remote[pid] = e
			}
		}
	} else {
		entries, err := p.ListEntries(actx)
		if err != nil {
			return err
		}
		s.storeList(ctx, user.ID, p.Name(), entries)
		for _, e := range entries {
			remote[e.Manga.ProviderID] = e
		}
	}
	settled := []int{progressPart, ratingPart, favouritePart}
	fav, canFav := p.(catalog.Favouriter)
	remoteFav := map[string]bool{}
	switch {
	case canFav && lookup:
		for pid, titleID := range tracked {
			remoteFav[pid] = v.localFav[pid]
			if saved[titleID].parts[favouritePart] != states[titleID][favouritePart] {
				on, err := fav.IsFavourite(actx, pid)
				if canFav = err == nil; !canFav {
					settled = settled[:2]
					break
				}
				remoteFav[pid] = on
			}
		}
	case canFav:
		ids, err := fav.Favourites(actx)
		if canFav = err == nil; !canFav {
			settled = settled[:2]
		}
		for _, id := range ids {
			remoteFav[id] = true
		}
	}
	failed, writes := map[int64]bool{}, map[string]*catalog.EntryUpdate{}
	handle := progress.Register(p.Name() + " sync")
	handle.SetTotal(len(tracked))
	defer handle.MarkDone()
	var errs []error
	n := 0
	for pid, titleID := range tracked {
		n++
		handle.Update(n, len(tracked), 0)
		on, t := v.enabled(titleID), v.titles[titleID]
		push, pull, rating := on && a.Source == SourceKaodoku, on && a.Source == SourceTracker, a.ratingFor(on)
		if !on && rating == "" || !ContentAllowed(user, t.IsAdult, t.ContentTags) {
			continue
		}
		switch {
		case !canFav || v.localFav[pid] == remoteFav[pid]:
		case push:
			if err := fav.ToggleFavourite(actx, pid); err != nil {
				errs, failed[titleID] = append(errs, fmt.Errorf("favourite %s: %w", pid, err)), true
			}
		case pull && s.sourceLeader(ctx, user.ID, titleID) == p.Name():
			errs = append(errs, s.lib.SetFavourite(ctx, user.ID, titleID, remoteFav[pid]))
		}
		e, listed := remote[pid]
		if pull && listed && e.Progress > 0 {
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
		if !listed {
			e.Score = -1
		}
		score := s.syncRating(ctx, user.ID, titleID, p.Name(), rating, e.Score)
		if !listed && score == 0 {
			if _, status, err := s.localTrackerState(ctx, user.ID, titleID); !push || err == nil && status == "PLANNING" && !ownsTitle(user.ID, v.owners[titleID]) {
				continue
			}
		}
		if sent, err := s.writeEntry(ctx, actx, p, pid, user.ID, titleID, e, listed, push, false, score); err != nil {
			errs, failed[titleID] = append(errs, fmt.Errorf("push %s: %w", pid, err)), true
		} else {
			writes[pid] = &sent
		}
	}
	s.patchList(ctx, user.ID, p.Name(), writes, tracked)
	for _, titleID := range tracked {
		if st, ok := states[titleID]; ok && !failed[titleID] {
			s.settle(ctx, user.ID, titleID, p.Name(), st, settled...)
		}
	}
	return errs2err(errs)
}

const maxEntryLookups = 2

const (
	progressPart = iota
	ratingPart
	favouritePart
)

var syncedColumns = [3]string{"progress", "rating", "favourite"}

type syncView struct {
	user     *auth.User
	account  TrackerAccount
	enabled  func(int64) bool
	titles   map[int64]library.Title
	owners   map[int64]int64
	localFav map[string]bool
}

func (s *JobService) syncView(ctx context.Context, user *auth.User, provider string, titleIDs []int64) (syncView, error) {
	v := syncView{user: user}
	v.account, _ = s.trackerAccount(ctx, user.ID, provider)
	var err error
	if v.enabled, err = s.progressiveTitles(ctx, user.ID, provider); err != nil {
		return v, err
	}
	titles, err := s.lib.repo.TitlesByIDs(ctx, titleIDs)
	if err != nil {
		return v, err
	}
	v.titles = make(map[int64]library.Title, len(titles))
	for _, t := range titles {
		v.titles[t.ID] = t
	}
	if v.owners, err = s.lib.TitleOwners(ctx); err != nil {
		return v, err
	}
	v.localFav, err = s.localFavouriteIDs(ctx, user.ID, provider)
	return v, err
}

// syncParts is a title's local state as a tracker is sent it, split into the progress, rating and favourite parts.
func (s *JobService) syncParts(ctx context.Context, v syncView, p catalog.Provider, pid string, titleID int64) [3]string {
	on, t := v.enabled(titleID), v.titles[titleID]
	allowed := ContentAllowed(v.user, t.IsAdult, t.ContentTags)
	local, _ := s.localUpdate(ctx, p, v.user.ID, titleID)
	r, rated := s.userRating(ctx, v.user.ID, titleID)
	return [3]string{
		fmt.Sprint(pid, on, allowed, ownsTitle(v.user.ID, v.owners[titleID]), local),
		fmt.Sprint(pid, v.account.ratingFor(on), allowed, r, rated),
		favPart(v, pid, titleID, v.localFav[pid]),
	}
}

func favPart(v syncView, pid string, titleID int64, fav bool) string {
	t := v.titles[titleID]
	return fmt.Sprint(pid, v.enabled(titleID), ContentAllowed(v.user, t.IsAdult, t.ContentTags), fav)
}

// syncedRow is what a title last settled at on a tracker: its parts, and the tracker's entry as last read and written.
type syncedRow struct {
	parts  [3]string
	remote string
}

func (s *JobService) syncedRows(ctx context.Context, userID int64, provider string, titleID int64) map[int64]syncedRow {
	out := map[int64]syncedRow{}
	rows, err := s.db.QueryContext(ctx, `SELECT title_id, progress, rating, favourite, remote FROM tracker_synced WHERE user_id = ? AND provider = ? AND ? IN (0, title_id)`, userID, provider, titleID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r syncedRow
		if rows.Scan(&id, &r.parts[0], &r.parts[1], &r.parts[2], &r.remote) == nil {
			out[id] = r
		}
	}
	return out
}

// titleState loads one title's view, current parts and settled row on a tracker, for a push.
func (s *JobService) titleState(ctx context.Context, userID, titleID int64, p catalog.Provider, pid string) (syncView, [3]string, syncedRow, error) {
	user, err := s.auth.GetUser(ctx, userID)
	if err != nil || user == nil {
		user = &auth.User{ID: userID}
	}
	v, err := s.syncView(ctx, user, p.Name(), []int64{titleID})
	if err != nil {
		return v, [3]string{}, syncedRow{}, err
	}
	return v, s.syncParts(ctx, v, p, pid, titleID), s.syncedRows(ctx, userID, p.Name(), titleID)[titleID], nil
}

func (s *JobService) settle(ctx context.Context, userID, titleID int64, provider string, st [3]string, parts ...int) {
	for _, i := range parts {
		col := syncedColumns[i]
		_, _ = s.db.ExecContext(ctx, `INSERT INTO tracker_synced (user_id, title_id, provider, `+col+`) VALUES (?, ?, ?, ?)
			ON CONFLICT (user_id, title_id, provider) DO UPDATE SET `+col+` = excluded.`+col, userID, titleID, provider, st[i])
	}
}

// remoteEntry trusts the entry kept from the last read and writes when only kaodoku updates the tracker, else reads it.
func remoteEntry(actx context.Context, p catalog.Provider, exclusive bool, kept, id string) (catalog.Entry, bool, error) {
	var e catalog.Entry
	if exclusive && kept != "" && json.Unmarshal([]byte(kept), &e) == nil {
		return e, true, nil
	}
	return p.GetEntry(actx, id)
}

// keepRemote stores the tracker's entry after a write, or forgets it when a write may have moved it (a new entry or status).
func (s *JobService) keepRemote(ctx context.Context, userID, titleID int64, provider string, e catalog.Entry, ok bool) {
	data := ""
	if ok {
		e.Manga = catalog.Manga{}
		b, _ := json.Marshal(e)
		data = string(b)
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO tracker_synced (user_id, title_id, provider, remote) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, title_id, provider) DO UPDATE SET remote = excluded.remote`, userID, titleID, provider, data)
}

func applyEntry(e catalog.Entry, u catalog.EntryUpdate) catalog.Entry {
	if u.Progress >= 0 {
		e.Progress = u.Progress
	}
	e.Status = cmp.Or(u.Status, e.Status)
	if u.Score != 0 {
		e.Score = max(u.Score, 0)
	}
	if !u.Started.IsZero() {
		e.Started = u.Started
	}
	if !u.Finished.IsZero() {
		e.Finished = u.Finished
	}
	return e
}

func (s *JobService) pushPending(userID, titleID int64) bool {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	return s.pushes[[2]int64{userID, titleID}] != nil
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
	out := catalog.EntryUpdate{Progress: -1, Remote: &remote}
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

// pushEntry writes one title and patches the stored list with it.
func (s *JobService) pushEntry(ctx, actx context.Context, p catalog.Provider, id string, userID, titleID int64, remote catalog.Entry, found, progress, exact bool, score int) error {
	sent, err := s.writeEntry(ctx, actx, p, id, userID, titleID, remote, found, progress, exact, score)
	if err == nil {
		s.patchList(ctx, userID, p.Name(), map[string]*catalog.EntryUpdate{id: &sent}, map[string]int64{id: titleID})
	}
	return err
}

// writeEntry raises a tracker's progress/status and fills empty dates when it syncs progressively, and sends score
// unless 0; exact also lowers progress after an explicit unread. It returns what it sent.
func (s *JobService) writeEntry(ctx, actx context.Context, p catalog.Provider, id string, userID, titleID int64, remote catalog.Entry, found, progress, exact bool, score int) (sent catalog.EntryUpdate, err error) {
	sent = catalog.EntryUpdate{Progress: -1}
	defer func() {
		if err == nil {
			s.keepRemote(ctx, userID, titleID, p.Name(), applyEntry(remote, sent), found && sent.Status == "")
		}
	}()
	if !progress && score == 0 {
		return sent, nil
	}
	local, err := s.localUpdate(ctx, p, userID, titleID)
	if err != nil {
		return sent, err
	}
	u := local
	if found {
		if !progress {
			local = catalog.EntryUpdate{Progress: -1}
		}
		u = entryDiff(local, remote, exact && progress)
	}
	u.Score = score
	if u.Empty() {
		return sent, nil
	}
	if err := p.SaveEntry(actx, id, u); err != nil {
		return sent, err
	}
	sent = u
	if score != 0 {
		return sent, s.setTrackerRating(ctx, userID, titleID, p.Name(), max(score, 0))
	}
	return sent, nil
}

func (s *JobService) PushTrackers(ctx context.Context, userID, titleID int64) {
	s.pushTrackers(ctx, userID, titleID, false)
}

func (s *JobService) PushTrackersExact(ctx context.Context, userID, titleID int64) {
	s.pushTrackers(ctx, userID, titleID, true)
}

const pushDelay = 10 * time.Minute

type pendingPush struct {
	ctx             context.Context
	userID, titleID int64
	exact           bool
	timer           *time.Timer
}

// pushTrackers sends a title's read progress once it stops changing for pushDelay.
func (s *JobService) pushTrackers(ctx context.Context, userID, titleID int64, exact bool) {
	key := [2]int64{userID, titleID}
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if p := s.pushes[key]; p != nil && p.timer.Stop() {
		exact = exact || p.exact
	}
	p := &pendingPush{ctx: context.WithoutCancel(ctx), userID: userID, titleID: titleID, exact: exact}
	p.timer = time.AfterFunc(pushDelay, func() { s.sendPush(p.ctx, p) })
	if s.pushes == nil {
		s.pushes = map[[2]int64]*pendingPush{}
	}
	s.pushes[key] = p
}

func (s *JobService) sendPush(ctx context.Context, p *pendingPush) {
	key := [2]int64{p.userID, p.titleID}
	s.pushMu.Lock()
	if s.pushes[key] == p {
		delete(s.pushes, key)
	}
	s.pushMu.Unlock()
	s.pushTitle(ctx, p.userID, p.titleID, true, p.exact)
}

// FlushPushes sends every pending push now, giving up after timeout.
func (s *JobService) FlushPushes(timeout time.Duration) {
	s.pushMu.Lock()
	var due []*pendingPush
	for _, p := range s.pushes {
		if p.timer.Stop() {
			due = append(due, p)
		}
	}
	s.pushMu.Unlock()
	var wg sync.WaitGroup
	for _, p := range due {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(p.ctx, timeout)
			defer cancel()
			s.sendPush(ctx, p)
		})
	}
	wg.Wait()
}

// pushTitle sends a title's progress, and any rating change with it, to its progressive trackers;
// without progress it sends only the rating, to the trackers kaodoku leads ratings for.
func (s *JobService) pushTitle(ctx context.Context, userID, titleID int64, progress, exact bool) {
	reason := "Rating push"
	if progress {
		reason = "Progress push"
	}
	ctx = catalog.WithReason(ctx, reason)
	v, _ := s.pushLocks.LoadOrStore([2]int64{userID, titleID}, &sync.Mutex{})
	lock := v.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	var names []string
	ratings := map[string]string{}
	for _, t := range s.titleSyncs(ctx, userID, titleID) {
		if progress && t.progress && t.source == SourceKaodoku || !progress && t.rating == SourceKaodoku {
			names, ratings[t.provider] = append(names, t.provider), t.rating
		}
	}
	m, ok := s.titleWork(ctx, titleID)
	if len(names) == 0 || !ok {
		return
	}
	s.eachTracker(ctx, userID, names, func(actx context.Context, p catalog.Provider) error {
		id := s.confirmedID(actx, m, p.Name())
		if id == "" {
			return nil
		}
		v, st, row, err := s.titleState(ctx, userID, titleID, p, id)
		if err != nil {
			return err
		}
		remote, found, err := remoteEntry(actx, p, v.account.Exclusive, row.remote, id)
		if err != nil {
			return err
		}
		score := s.syncRating(ctx, userID, titleID, p.Name(), ratings[p.Name()], remote.Score)
		if err := s.pushEntry(ctx, actx, p, id, userID, titleID, remote, found, progress, exact, score); err != nil {
			return err
		}
		settled := []int{ratingPart}
		if progress {
			settled = append(settled, progressPart)
		}
		if _, ok := p.(catalog.Favouriter); !ok {
			settled = append(settled, favouritePart)
		}
		s.settle(ctx, userID, titleID, p.Name(), st, settled...)
		return nil
	})
}

func (s *JobService) eachTracker(ctx context.Context, userID int64, names []string, fn func(context.Context, catalog.Provider) error) {
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
}

func (s *JobService) dropFromTrackers(ctx context.Context, userID int64, names []string, m catalog.Manga, remove bool) {
	ctx = catalog.WithReason(ctx, "Title removed")
	go s.eachTracker(context.WithoutCancel(ctx), userID, names, func(actx context.Context, p catalog.Provider) error {
		id := s.confirmedID(actx, m, p.Name())
		if id == "" {
			return nil
		}
		var u *catalog.EntryUpdate
		var err error
		if remove {
			err = p.DeleteEntry(actx, id)
		} else {
			u = &catalog.EntryUpdate{Progress: -1, Status: "DROPPED"}
			err = p.SaveEntry(actx, id, *u)
		}
		if err == nil {
			s.patchList(ctx, userID, p.Name(), map[string]*catalog.EntryUpdate{id: u}, nil)
		}
		return err
	})
}

func (s *JobService) pushFavourite(ctx context.Context, userID, titleID int64, want bool) {
	ctx = catalog.WithReason(ctx, "Favourite push")
	ctx = context.WithoutCancel(ctx)
	m, ok := s.titleWork(ctx, titleID)
	if !ok {
		return
	}
	var names []string
	for _, t := range s.titleSyncs(ctx, userID, titleID) {
		if t.progress && t.source == SourceKaodoku {
			names = append(names, t.provider)
		}
	}
	go s.eachTracker(ctx, userID, names, func(actx context.Context, p catalog.Provider) error {
		fav, ok := p.(catalog.Favouriter)
		id := s.confirmedID(actx, m, p.Name())
		if !ok || id == "" {
			return nil
		}
		v, _, row, err := s.titleState(ctx, userID, titleID, p, id)
		if err != nil {
			return err
		}
		remote := !want
		if !v.account.Exclusive || row.parts[favouritePart] != favPart(v, id, titleID, remote) {
			if remote, err = fav.IsFavourite(actx, id); err != nil {
				return err
			}
		}
		if remote != want {
			if err := fav.ToggleFavourite(actx, id); err != nil {
				return err
			}
		}
		s.settle(ctx, userID, titleID, p.Name(), [3]string{favouritePart: favPart(v, id, titleID, want)}, favouritePart)
		return nil
	})
}

// forgetTrackerState drops the ratings last seen on, and states synced to, a tracker, so a (re)connected account starts fresh.
func (s *JobService) forgetTrackerState(ctx context.Context, userID int64, provider string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tracker_ratings WHERE user_id = ? AND provider = ?;
		DELETE FROM tracker_synced WHERE user_id = ? AND provider = ?`, userID, provider, userID, provider)
	return err
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
	ctx = catalog.WithReason(ctx, "Title sync")
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
	sources, ratings := map[string]string{}, map[string]string{}
	for _, a := range s.TrackerAccounts(ctx, userID) {
		sources[a.Provider] = a.Source
	}
	for _, t := range s.titleSyncs(ctx, userID, titleID) {
		ratings[t.provider] = t.rating
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
		errs = append(errs, s.trackerAuthError(ctx, userID, name, s.syncTitle(ctx, actx, p, id, userID, titleID, sources[name], ratings[name])))
	}
	if linked == 0 {
		return fmt.Errorf("title is not linked on any connected tracker")
	}
	s.invalidateRecs(userID)
	return errors.Join(errs...)
}

// SetProgressiveSync stores an explicit per-title choice, which overrides the account's auto progressive default.
func (s *JobService) SetProgressiveSync(ctx context.Context, titleID int64, provider string, on bool) error {
	if !slices.Contains(providerOrder, provider) {
		return fmt.Errorf("unknown tracker %q", provider)
	}
	if on {
		if err := s.SyncTrackersTitle(ctx, titleID, provider); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tracker_title_sync (user_id, title_id, provider, enabled) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, title_id, provider) DO UPDATE SET enabled = excluded.enabled`, auth.UserID(ctx), titleID, provider, on)
	return err
}

type titleSync struct {
	provider, rating, source string
	progress                 bool
	position                 int
}

// titleSyncs lists the trackers syncing a title's progress or rating.
func (s *JobService) titleSyncs(ctx context.Context, userID, titleID int64) []titleSync {
	var out []titleSync
	for _, a := range s.TrackerAccounts(ctx, userID) {
		if !s.providerConfigured(ctx, a.Provider) || !auth.FromContext(ctx).ProviderAllowed(a.Provider) {
			continue
		}
		on := a.AutoProgressive
		_ = s.db.QueryRowContext(ctx, `SELECT enabled FROM tracker_title_sync WHERE user_id = ? AND title_id = ? AND provider = ?`, userID, titleID, a.Provider).Scan(&on)
		if t := (titleSync{a.Provider, a.ratingFor(on), a.Source, on, a.Position}); t.progress || t.rating != "" {
			out = append(out, t)
		}
	}
	return out
}

func (s *JobService) progressiveTrackers(ctx context.Context, userID, titleID int64) []string {
	var out []string
	for _, t := range s.titleSyncs(ctx, userID, titleID) {
		if t.progress {
			out = append(out, t.provider)
		}
	}
	return out
}

type TitleRating struct {
	Rating  int           `json:"rating"`
	From    string        `json:"from,omitempty"`
	Missing []ProviderRef `json:"missing,omitempty"`
}

type ProviderRef struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
}

// TitleRating is the acting user's rating, 0 for none, the tracker it is taken from when one leads it,
// and the leading trackers with no rating for it that still need an answer.
func (s *JobService) TitleRating(ctx context.Context, titleID int64) TitleRating {
	uid := auth.UserID(ctx)
	r, _ := s.userRating(ctx, uid, titleID)
	out := TitleRating{Rating: r}
	if p, _ := s.ratingLeader(ctx, uid, titleID); p != "" {
		out.From = s.ProviderLabel(p)
	}
	for _, t := range s.titleSyncs(ctx, uid, titleID) {
		if t.rating == SourceTracker && r > 0 && s.unratedOn(ctx, uid, titleID, t.provider) {
			out.Missing = append(out.Missing, ProviderRef{t.provider, s.ProviderLabel(t.provider)})
		}
	}
	return out
}

func (s *JobService) unratedOn(ctx context.Context, userID, titleID int64, provider string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tracker_ratings WHERE user_id = ? AND title_id = ? AND provider = ? AND rating = 0 AND choice = ''`, userID, titleID, provider).Scan(&n)
	return n > 0
}

type MissingRating struct {
	library.Title
	Rating int
}

// MissingRatings lists titles rated in Kaodoku that a tracker leading their ratings has none for.
func (s *JobService) MissingRatings(ctx context.Context, provider string) ([]MissingRating, error) {
	uid := auth.UserID(ctx)
	rows, err := s.db.QueryContext(ctx, `SELECT ur.title_id, ur.rating FROM tracker_ratings tr JOIN user_ratings ur ON ur.user_id = tr.user_id AND ur.title_id = tr.title_id
		WHERE tr.user_id = ? AND tr.provider = ? AND tr.rating = 0 AND tr.choice = '' AND ur.rating > 0`, uid, provider)
	if err != nil {
		return nil, err
	}
	ratings := map[int64]int{}
	var ids []int64
	for rows.Next() {
		var id int64
		var r int
		if rows.Scan(&id, &r) == nil {
			ids, ratings[id] = append(ids, id), r
		}
	}
	rows.Close()
	titles, err := s.lib.repo.TitlesByIDs(ctx, ids)
	var out []MissingRating
	for _, t := range titles {
		if slices.ContainsFunc(s.titleSyncs(ctx, uid, t.ID), func(ts titleSync) bool { return ts.provider == provider && ts.rating == SourceTracker }) {
			out = append(out, MissingRating{t, ratings[t.ID]})
		}
	}
	slices.SortFunc(out, func(a, b MissingRating) int {
		return strings.Compare(strings.ToLower(a.DisplayTitle), strings.ToLower(b.DisplayTitle))
	})
	return out, err
}

// AnswerMissingRating sends the Kaodoku rating to a tracker that has none, or keeps it unrated there.
func (s *JobService) AnswerMissingRating(ctx context.Context, titleID int64, provider string, send bool) error {
	uid := auth.UserID(ctx)
	if !send {
		_, err := s.db.ExecContext(ctx, `UPDATE tracker_ratings SET choice = 'keep' WHERE user_id = ? AND title_id = ? AND provider = ?`, uid, titleID, provider)
		return err
	}
	ctx = catalog.WithReason(ctx, "Rating push")
	m, ok := s.titleWork(ctx, titleID)
	p, actx, connected := s.trackerSession(ctx, uid, provider)
	id := s.confirmedID(ctx, m, provider)
	if !ok || !connected || id == "" {
		return fmt.Errorf("%s is not linked for this title", s.ProviderLabel(provider))
	}
	r, _ := s.userRating(ctx, uid, titleID)
	remote, found, err := p.GetEntry(actx, id)
	if err != nil {
		return s.trackerAuthError(ctx, uid, provider, err)
	}
	return s.trackerAuthError(ctx, uid, provider, s.pushEntry(ctx, actx, p, id, uid, titleID, remote, found, false, false, r))
}

// SetTitleRating stores the acting user's rating, 0 for none, and sends it to the trackers kaodoku leads.
func (s *JobService) SetTitleRating(ctx context.Context, titleID int64, rating int) error {
	if rating < 0 || rating > 10 {
		return fmt.Errorf("ratings go from 1 to 10")
	}
	uid := auth.UserID(ctx)
	if err := s.storeRating(ctx, uid, titleID, rating); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tracker_ratings SET choice = '' WHERE user_id = ? AND title_id = ?`, uid, titleID); err != nil {
		return err
	}
	go s.pushTitle(context.WithoutCancel(ctx), uid, titleID, false, false)
	return nil
}

// syncRating returns the score a kaodoku-led tracker needs, or takes in a tracker-led one's rating; remote < 0 is unknown.
func (s *JobService) syncRating(ctx context.Context, userID, titleID int64, provider, from string, remote int) int {
	if from == "" {
		return 0
	}
	seen, known := s.trackerRating(ctx, userID, titleID, provider)
	switch {
	case remote >= 0 && (!known || remote != seen):
		_ = s.setTrackerRating(ctx, userID, titleID, provider, remote)
	case remote < 0 && known:
		remote = seen
	}
	if from == SourceTracker {
		if p, r := s.ratingLeader(ctx, userID, titleID); p != "" {
			if local, set := s.userRating(ctx, userID, titleID); (!set || local != r) && s.storeRating(ctx, userID, titleID, r) == nil {
				go s.pushTitle(context.WithoutCancel(ctx), userID, titleID, false, false)
			}
		}
		return 0
	}
	local, set := s.userRating(ctx, userID, titleID)
	if !set || local == remote {
		return 0
	}
	return cmp.Or(local, catalog.ScoreClear)
}

// sourceLeader is the tracker that is the source of a title it syncs and comes first in the user's precedence.
func (s *JobService) sourceLeader(ctx context.Context, userID, titleID int64) string {
	leader, first := "", 0
	for _, t := range s.titleSyncs(ctx, userID, titleID) {
		if t.progress && t.source == SourceTracker && (leader == "" || t.position < first) {
			leader, first = t.provider, t.position
		}
	}
	return leader
}

// ratingLeader is the tracker ratings come from that holds a rating for the title and comes first in the user's precedence.
func (s *JobService) ratingLeader(ctx context.Context, userID, titleID int64) (string, int) {
	leader, rating, first := "", 0, 0
	for _, t := range s.titleSyncs(ctx, userID, titleID) {
		if r, ok := s.trackerRating(ctx, userID, titleID, t.provider); t.rating == SourceTracker && ok && r > 0 && (leader == "" || t.position < first) {
			leader, rating, first = t.provider, r, t.position
		}
	}
	return leader, rating
}

func (s *JobService) userRating(ctx context.Context, userID, titleID int64) (int, bool) {
	var r int
	err := s.db.QueryRowContext(ctx, `SELECT rating FROM user_ratings WHERE user_id = ? AND title_id = ?`, userID, titleID).Scan(&r)
	return r, err == nil
}

func (s *JobService) storeRating(ctx context.Context, userID, titleID int64, rating int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_ratings (user_id, title_id, rating) VALUES (?, ?, ?)
		ON CONFLICT (user_id, title_id) DO UPDATE SET rating = excluded.rating, updated_at = CURRENT_TIMESTAMP`, userID, titleID, rating)
	return err
}

// trackerRating is the rating last seen on, or sent to, a tracker.
func (s *JobService) trackerRating(ctx context.Context, userID, titleID int64, provider string) (int, bool) {
	var r int
	err := s.db.QueryRowContext(ctx, `SELECT rating FROM tracker_ratings WHERE user_id = ? AND title_id = ? AND provider = ?`, userID, titleID, provider).Scan(&r)
	return r, err == nil
}

func (s *JobService) setTrackerRating(ctx context.Context, userID, titleID int64, provider string, rating int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tracker_ratings (user_id, title_id, provider, rating) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, title_id, provider) DO UPDATE SET rating = excluded.rating`, userID, titleID, provider, rating)
	return err
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

func (s *JobService) syncTitle(ctx, actx context.Context, p catalog.Provider, id string, userID, titleID int64, source, rating string) error {
	remote, found, err := p.GetEntry(actx, id)
	if err != nil {
		return err
	}
	if local, _, err := s.localTrackerState(ctx, userID, titleID); err != nil {
		return err
	} else if source == SourceTracker && found && remote.Progress > local {
		if _, err := s.lib.MarkChaptersReadThrough(ctx, titleID, remote.Progress); err != nil {
			return err
		}
	}
	score := s.syncRating(ctx, userID, titleID, p.Name(), rating, remote.Score)
	return s.pushEntry(ctx, actx, p, id, userID, titleID, remote, found, source == SourceKaodoku, false, score)
}

// TrackerSuggestions merges the user's lists on provider, or on every reachable connected tracker when empty.
func (s *JobService) TrackerSuggestions(ctx context.Context, provider string) []catalog.Manga {
	ctx = catalog.WithReason(ctx, "Your lists")
	uid := auth.UserID(ctx)
	names := slices.DeleteFunc(s.connectedTrackers(ctx, uid), func(name string) bool { return provider != "" && name != provider })
	lists := make([][]catalog.Manga, len(names))
	for i, entries := range s.trackerLists(ctx, uid, names) {
		for _, e := range entries {
			if e.Status != "DROPPED" {
				lists[i] = append(lists[i], e.Manga)
			}
		}
		lists[i] = s.fillFromStore(ctx, names[i], lists[i])
	}
	return mergeManga(lists, nil, 0)
}

// trackerLists downloads the user's lists on several trackers side by side; a failed one comes back empty.
func (s *JobService) trackerLists(ctx context.Context, uid int64, names []string) [][]catalog.Entry {
	out := make([][]catalog.Entry, len(names))
	parallel(ctx, len(names), len(names), func(i int) {
		out[i], _ = s.cachedEntries(ctx, uid, names[i])
	})
	return out
}

// fillFromStore swaps thin records (no cover, or no genres to judge adult content by) for the stored copy when there is one.
func (s *JobService) fillFromStore(ctx context.Context, provider string, items []catalog.Manga) []catalog.Manga {
	ids := make([]string, len(items))
	for i, m := range items {
		ids[i] = m.ProviderID
	}
	stored, _ := s.catalog.repo.MangaByProviderIDs(ctx, provider, ids)
	for i, m := range items {
		if known, ok := stored[m.ProviderID]; ok && m.Provider == provider && (m.CoverImage == "" || len(m.Genres)+len(m.Tags) == 0) {
			known.ExternalIDs, known.RelationType = m.ExternalIDs, m.RelationType
			items[i] = known
		}
	}
	return items
}

type cachedList struct {
	entries []catalog.Entry
	fetched time.Time
}

func listKey(uid int64, provider string) string { return fmt.Sprintf("%d:%s", uid, provider) }

// cachedEntries serves a user's list from memory or the database and downloads it only when there is none yet;
// syncs replace the stored copy and Kaodoku's own writes patch it, so pages never download it again.
func (s *JobService) cachedEntries(ctx context.Context, uid int64, provider string) ([]catalog.Entry, error) {
	if c, ok := s.cachedList(ctx, uid, provider); ok {
		return c.entries, nil
	}
	v, _ := s.listLocks.LoadOrStore(listKey(uid, provider), &sync.Mutex{})
	lock := v.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	if c, ok := s.cachedList(ctx, uid, provider); ok {
		return c.entries, nil
	}
	return s.fetchList(ctx, uid, provider)
}

func (s *JobService) cachedList(ctx context.Context, uid int64, provider string) (cachedList, bool) {
	key := listKey(uid, provider)
	s.listMu.Lock()
	c, ok := s.listCache[key]
	s.listMu.Unlock()
	if ok {
		return c, true
	}
	var data, fetched string
	if s.db.QueryRowContext(ctx, `SELECT list_json, list_fetched_at FROM tracker_accounts WHERE user_id = ? AND provider = ? AND list_json != ''`, uid, provider).Scan(&data, &fetched) != nil {
		return c, false
	}
	t, err := database.ParseTime(fetched)
	if err != nil || json.Unmarshal([]byte(data), &c.entries) != nil {
		return cachedList{}, false
	}
	c.fetched = t
	s.keepList(key, c)
	return c, true
}

func (s *JobService) keepList(key string, c cachedList) {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	if s.listCache == nil {
		s.listCache = map[string]cachedList{}
	}
	s.listCache[key] = c
}

func (s *JobService) forgetList(uid int64, provider string) {
	s.listMu.Lock()
	delete(s.listCache, listKey(uid, provider))
	s.listMu.Unlock()
}

func (s *JobService) fetchList(ctx context.Context, uid int64, provider string) ([]catalog.Entry, error) {
	p, actx, ok := s.trackerSession(ctx, uid, provider)
	if !ok {
		return nil, fmt.Errorf("%s is not connected", provider)
	}
	entries, err := p.ListEntries(actx)
	if err != nil {
		return nil, err
	}
	s.storeList(ctx, uid, provider, entries)
	return entries, nil
}

// storeList keeps a downloaded list in memory and in the database, so a restart does not download it again.
func (s *JobService) storeList(ctx context.Context, uid int64, provider string, entries []catalog.Entry) {
	s.saveList(ctx, uid, provider, cachedList{entries, time.Now()})
}

func (s *JobService) saveList(ctx context.Context, uid int64, provider string, c cachedList) {
	s.keepList(listKey(uid, provider), c)
	if data, err := json.Marshal(c.entries); err == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE tracker_accounts SET list_json = ?, list_fetched_at = ? WHERE user_id = ? AND provider = ?`, data, database.FormatTime(c.fetched), uid, provider)
	}
	s.trimLists(ctx)
}

// patchList applies writes Kaodoku made to the stored list, adding titles (by id in titles) that weren't listed;
// a nil write removes the entry.
func (s *JobService) patchList(ctx context.Context, uid int64, provider string, writes map[string]*catalog.EntryUpdate, titles map[string]int64) {
	c, ok := s.cachedList(ctx, uid, provider)
	if !ok {
		return
	}
	entries, changed := slices.Clone(c.entries), false
	for id, u := range writes {
		if u != nil && u.Empty() {
			continue
		}
		i := slices.IndexFunc(entries, func(e catalog.Entry) bool { return e.Manga.ProviderID == id })
		switch {
		case u == nil && i >= 0:
			entries, changed = slices.Delete(entries, i, i+1), true
		case u == nil:
		case i >= 0:
			entries[i], changed = applyEntry(entries[i], *u), true
		default:
			if m, ok := s.titleWork(ctx, titles[id]); ok {
				m.Provider, m.ProviderID = provider, id
				entries, changed = append(entries, applyEntry(catalog.Entry{Manga: m}, *u)), true
			}
		}
	}
	if changed {
		s.saveList(ctx, uid, provider, cachedList{entries, c.fetched})
	}
}

// ProviderSuggestions approximates a backend's own suggestions from community recommendations for the user's reading
// on it; ok is false when not connected. Aggressive MangaBaka mode asks the tracker only when fetch is set, and
// fetchable says whether that would add anything.
func (s *JobService) ProviderSuggestions(ctx context.Context, provider string, fetch bool) (items []catalog.Manga, fetchable, ok bool) {
	ctx = catalog.WithReason(ctx, "List suggestions")
	uid := auth.UserID(ctx)
	key := listKey(uid, provider)
	s.recMu.Lock()
	c, cached := s.sugCache[key]
	s.recMu.Unlock()
	if cached && !fetch && time.Now().Before(c.expires) {
		return c.items, c.fetchable, true
	}
	entries, err := s.cachedEntries(ctx, uid, provider)
	if err != nil {
		return nil, false, false
	}
	p, err := s.Provider(ctx, provider)
	if err != nil {
		return nil, false, false
	}
	aggressive := s.mbAggressive(ctx) && !fetch && provider != catalog.MangaBakaProvider
	exclude := map[string]bool{}
	for k := range s.LibraryIndex(ctx) {
		exclude[k] = true
	}
	var seeds []string
	for _, e := range entries {
		for _, k := range catalog.Keys(e.Manga) {
			exclude[k] = true
		}
		if (recommends(provider) || aggressive) && (e.Status == "CURRENT" || e.Status == "COMPLETED" || e.Status == "REPEATING") {
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
		if !aggressive {
			if _, recs, err := s.relatedCached(ctx, p, id, !s.mbAggressive(ctx), true); err == nil {
				lists = append(lists, s.mbUnify(ctx, recs))
			}
			continue
		}
		if sr, ok := s.mb.ByProvider(ctx, provider, id); ok {
			rels, recs := s.mb.Related(ctx, sr.ID, 24)
			lists = append(lists, s.mbMangas(ctx, recs), s.mbMangas(ctx, rels))
		}
		_, recs, _, fetched := s.relatedStored(ctx, provider, id)
		fetchable = fetchable || recommends(provider) && relatedStale(fetched)
		lists = append(lists, s.mbUnify(ctx, recs))
	}
	items = s.fillFromStore(ctx, provider, mergeManga(lists, exclude, 0))
	s.recMu.Lock()
	if s.sugCache == nil {
		s.sugCache = map[string]cachedRecs{}
	}
	s.sugCache[key] = cachedRecs{items: items, fetchable: fetchable, expires: time.Now().Add(recommendationTTL)}
	trimOldest(s.sugCache, s.cacheLimit(ctx, SettingCacheRecs), cachedRecs.expiry)
	s.recMu.Unlock()
	return items, fetchable, true
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
