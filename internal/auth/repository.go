package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/brogergvhs/kaodoku/internal/database"
)

const sessionTTL = 30 * 24 * time.Hour

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Service manages users, roles and sessions.
type Service struct {
	db      database.Handle
	touched sync.Map // session token hash -> last_seen_at write
	loginMu sync.Mutex
	logins  map[string]*loginState
}

type loginState struct {
	fails int
	until time.Time
}

const (
	loginMaxFails   = 5
	loginLockWindow = 15 * time.Minute
)

func NewService(db database.Handle) *Service {
	return &Service{db: db, logins: map[string]*loginState{}}
}

// loginAllowed rejects a username that has hit the failed-attempt cap within
// the lock window.
func (s *Service) loginAllowed(username string) error {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if st := s.logins[username]; st != nil && st.fails >= loginMaxFails && time.Now().Before(st.until) {
		return fmt.Errorf("too many failed attempts, try again later")
	}
	return nil
}

func (s *Service) loginFailed(username string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	st := s.logins[username]
	if st == nil {
		st = &loginState{}
		s.logins[username] = st
	}
	st.fails++
	st.until = time.Now().Add(loginLockWindow)
}

func (s *Service) loginReset(username string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.logins, username)
}

// Bootstrap seeds built-in roles and the env admin (user id 1). It runs at
// every startup so the admin credentials always mirror the environment and
// the admin role always covers newly added permissions.
func (s *Service) Bootstrap(ctx context.Context, adminUser, adminPassword string) error {
	for _, role := range builtinRoles() {
		perms, _ := json.Marshal(role.Perms)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO roles (name, origin, permissions_json) VALUES (?, 'builtin', ?)
			ON CONFLICT(name) DO UPDATE SET permissions_json = excluded.permissions_json
			WHERE roles.origin = 'builtin'
		`, role.Name, string(perms)); err != nil {
			return fmt.Errorf("seed role %s: %w", role.Name, err)
		}
	}
	adminUser = strings.TrimSpace(adminUser)
	if adminUser == "" {
		adminUser = "admin"
	}
	hash := ""
	if adminPassword != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash admin password: %w", err)
		}
		hash = string(h)
	}
	var adminRole int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM roles WHERE name = 'admin'`).Scan(&adminRole); err != nil {
		return fmt.Errorf("load admin role: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role_id, origin, allow_adult)
		VALUES (?, ?, ?, ?, 'env', 1)
		ON CONFLICT(id) DO UPDATE SET
			username = excluded.username,
			password_hash = excluded.password_hash,
			role_id = excluded.role_id,
			origin = 'env'
	`, EnvAdminID, adminUser, hash, adminRole); err != nil {
		return fmt.Errorf("bootstrap env admin: %w", err)
	}
	return nil
}

// Login verifies credentials and returns a new session token.
// Authenticate verifies credentials (with lockout) and returns the user id,
// without creating a session — used by session login and API-token minting.
func (s *Service) Authenticate(ctx context.Context, username, password string) (int64, error) {
	username = strings.TrimSpace(username)
	if err := s.loginAllowed(username); err != nil {
		return 0, err
	}
	var id int64
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && hash == "") {
		s.loginFailed(username)
		return 0, fmt.Errorf("invalid credentials")
	}
	if err != nil {
		return 0, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.loginFailed(username)
		return 0, fmt.Errorf("invalid credentials")
	}
	s.loginReset(username)
	return id, nil
}

func (s *Service) Login(ctx context.Context, username, password, userAgent, ip string) (string, error) {
	id, err := s.Authenticate(ctx, username, password)
	if err != nil {
		return "", err
	}
	return s.IssueSession(ctx, id, userAgent, ip)
}

// IssueSession mints a session for an already-authenticated user id.
func (s *Service) IssueSession(ctx context.Context, id int64, userAgent, ip string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := time.Now().Add(sessionTTL)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at, user_agent, ip, last_seen_at) VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, hex.EncodeToString(sum[:]), id, database.FormatTime(expires), userAgent, ip); err != nil {
		return "", fmt.Errorf("store session: %w", err)
	}
	return token, nil
}

// UserByOIDCSubject resolves a linked OIDC subject to its user, or nil.
func (s *Service) UserByOIDCSubject(ctx context.Context, subject string) (*User, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, nil
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE oidc_subject = ?`, subject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, id)
}

// CreateOIDCUser provisions a password-less IdP-owned user (empty hash blocks
// password login); pending users cannot use the app until approved.
func (s *Service) CreateOIDCUser(ctx context.Context, username, subject string, roleID int64, pending bool) (int64, error) {
	username, err := validUsername(username)
	subject = strings.TrimSpace(subject)
	if err != nil || subject == "" {
		return 0, fmt.Errorf("a valid username and subject are required")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, role_id, origin, oidc_subject, pending) VALUES (?, '', ?, ?, ?, ?)
	`, username, roleID, OriginOIDC, subject, boolInt(pending))
	if err != nil {
		return 0, fmt.Errorf("create oidc user %q: %w", username, err)
	}
	return res.LastInsertId()
}

// CountPendingUsers reports how many accounts await approval.
func (s *Service) CountPendingUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE pending = 1`).Scan(&n)
	return n, err
}

// SetUserPending approves (false) or suspends into approval (true) a user.
func (s *Service) SetUserPending(ctx context.Context, userID int64, pending bool) error {
	if userID == EnvAdminID {
		return fmt.Errorf("the environment admin cannot be pending")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET pending = ? WHERE id = ?`, boolInt(pending), userID)
	return err
}

// SetOIDCSubject links (or with "" unlinks) an IdP subject to a user.
func (s *Service) SetOIDCSubject(ctx context.Context, userID int64, subject string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET oidc_subject = ? WHERE id = ?`, strings.TrimSpace(subject), userID)
	return err
}

// SetUserRole updates a user's role; the env admin is immutable.
func (s *Service) SetUserRole(ctx context.Context, userID, roleID int64) error {
	if userID == EnvAdminID {
		return fmt.Errorf("the environment admin's role is fixed")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role_id = ? WHERE id = ?`, roleID, userID)
	return err
}

// UserBySession resolves a session token to its user, or nil when invalid.
// Successful resolves touch last_seen_at, throttled to one write per minute
// per session (SQLite runs on a single connection).
func (s *Service) UserBySession(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	var id int64
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`,
		tokenHash).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if t, err := database.ParseTime(expires); err != nil || time.Now().After(t) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
		return nil, nil
	}
	s.touchSession(ctx, tokenHash)
	return s.GetUser(ctx, id)
}

func (s *Service) touchSession(ctx context.Context, tokenHash string) {
	if last, ok := s.touched.Load(tokenHash); ok && time.Since(last.(time.Time)) < time.Minute {
		return
	}
	s.touched.Store(tokenHash, time.Now())
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = CURRENT_TIMESTAMP WHERE token_hash = ?`, tokenHash)
}

// ActiveSession is one unexpired login for the global session viewer.
type ActiveSession struct {
	TokenHash  string
	UserID     int64
	Username   string
	UserAgent  string
	IP         string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// ActiveSessions lists all unexpired sessions, most recently seen first.
func (s *Service) ActiveSessions(ctx context.Context) ([]ActiveSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT se.token_hash, se.user_id, u.username, se.user_agent, se.ip, se.created_at, COALESCE(se.last_seen_at, se.created_at)
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.expires_at > ?
		ORDER BY COALESCE(se.last_seen_at, se.created_at) DESC`, database.FormatTime(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveSession
	for rows.Next() {
		var a ActiveSession
		var created, seen string
		if err := rows.Scan(&a.TokenHash, &a.UserID, &a.Username, &a.UserAgent, &a.IP, &created, &seen); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = database.ParseTime(created)
		a.LastSeenAt, _ = database.ParseTime(seen)
		out = append(out, a)
	}
	return out, rows.Err()
}

// PurgeExpiredSessions deletes expired sessions in bulk.
func (s *Service) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, database.FormatTime(time.Now()))
	return err
}

// Logout deletes the session for a token.
func (s *Service) Logout(ctx context.Context, token string) {
	sum := sha256.Sum256([]byte(token))
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hex.EncodeToString(sum[:]))
}

const userSelect = `
	SELECT u.id, u.username, u.origin, u.allow_adult, u.blocked_tags, u.allowed_tags, u.oidc_subject, u.pending, u.created_at, r.id, r.name, r.permissions_json
	FROM users u JOIN roles r ON r.id = u.role_id`

// GetUser loads one user with its role and permissions.
func (s *Service) GetUser(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE u.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func scanUser(row database.Scanner) (*User, error) {
	var u User
	var created, perms, blocked, allowed string
	var allowAdult, pending int
	if err := row.Scan(&u.ID, &u.Username, &u.Origin, &allowAdult, &blocked, &allowed, &u.OIDCSubject, &pending, &created, &u.RoleID, &u.RoleName, &perms); err != nil {
		return nil, err
	}
	u.AllowAdult = allowAdult != 0
	u.Pending = pending != 0
	_ = json.Unmarshal([]byte(blocked), &u.BlockedTags)
	_ = json.Unmarshal([]byte(allowed), &u.AllowedTags)
	u.CreatedAt, _ = database.ParseTime(created)
	var list []string
	_ = json.Unmarshal([]byte(perms), &list)
	u.Perms = make(map[string]bool, len(list))
	for _, p := range list {
		u.Perms[p] = true
	}
	return &u, nil
}

// ListUsers returns all users with role names.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, userSelect+` ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// ContentGuards is a user's per-user content restriction set.
type ContentGuards struct {
	AllowAdult  bool
	BlockedTags []string
	AllowedTags []string
}

// CreateUser adds a local user.
func validUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 64 {
		return "", fmt.Errorf("username must be 1-64 characters")
	}
	for _, r := range username {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("username contains control characters")
		}
	}
	return username, nil
}

func (s *Service) CreateUser(ctx context.Context, username, password string, roleID int64, guards ContentGuards) error {
	username, err := validUsername(username)
	if err != nil || len(password) < 4 {
		return fmt.Errorf("a valid username and a password of at least 4 characters are required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	blocked, _ := json.Marshal(guards.BlockedTags)
	allowed, _ := json.Marshal(guards.AllowedTags)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, role_id, origin, allow_adult, blocked_tags, allowed_tags) VALUES (?, ?, ?, 'local', ?, ?, ?)
	`, username, string(hash), roleID, boolInt(guards.AllowAdult), string(blocked), string(allowed)); err != nil {
		return fmt.Errorf("create user %q: %w", username, err)
	}
	return nil
}

// UpdateUser changes a local user's role and optionally password.
func (s *Service) UpdateUser(ctx context.Context, id int64, roleID int64, password string, guards ContentGuards) error {
	blocked, _ := json.Marshal(guards.BlockedTags)
	allowed, _ := json.Marshal(guards.AllowedTags)
	if id == EnvAdminID {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET allow_adult = ?, blocked_tags = ?, allowed_tags = ? WHERE id = ?`, boolInt(guards.AllowAdult), string(blocked), string(allowed), id)
		return err
	}
	if password != "" {
		if len(password) < 4 {
			return fmt.Errorf("password must be at least 4 characters")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `UPDATE users SET role_id = ?, allow_adult = ?, blocked_tags = ?, allowed_tags = ?, password_hash = ? WHERE id = ? AND origin != 'env'`, roleID, boolInt(guards.AllowAdult), string(blocked), string(allowed), string(hash), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role_id = ?, allow_adult = ?, blocked_tags = ?, allowed_tags = ? WHERE id = ? AND origin != 'env'`, roleID, boolInt(guards.AllowAdult), string(blocked), string(allowed), id)
	return err
}

// DeleteUser removes a local user (never the env admin) and their sessions.
func (s *Service) DeleteUser(ctx context.Context, id int64) error {
	if id == EnvAdminID {
		return fmt.Errorf("the environment admin cannot be deleted")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND origin != 'env'`, id); err != nil {
		return err
	}
	for _, table := range []string{"sessions", "user_favourites", "chapter_read_progress", "chapter_read_pages", "volume_read_progress"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ListRoles returns all roles.
func (s *Service) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, origin, permissions_json FROM roles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		var perms string
		if err := rows.Scan(&r.ID, &r.Name, &r.Origin, &perms); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(perms), &r.Perms)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveRole creates or updates a custom role. Built-in roles are immutable.
func (s *Service) SaveRole(ctx context.Context, id int64, name string, perms []string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("role name is required")
	}
	valid := map[string]bool{}
	for _, p := range Permissions() {
		valid[p] = true
	}
	clean := perms[:0]
	for _, p := range perms {
		if valid[p] {
			clean = append(clean, p)
		}
	}
	data, _ := json.Marshal(clean)
	if id == 0 {
		_, err := s.db.ExecContext(ctx, `INSERT INTO roles (name, origin, permissions_json) VALUES (?, 'local', ?)`, name, string(data))
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE roles SET name = ?, permissions_json = ? WHERE id = ? AND origin = 'local'`, name, string(data), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("built-in roles cannot be edited")
	}
	return nil
}

// DeleteRole removes an unused custom role.
func (s *Service) DeleteRole(ctx context.Context, id int64) error {
	var users int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role_id = ?`, id).Scan(&users); err != nil {
		return err
	}
	if users > 0 {
		return fmt.Errorf("role is still assigned to %d user(s)", users)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM roles WHERE id = ? AND origin = 'local'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("built-in roles cannot be deleted")
	}
	return nil
}

// ChangePassword lets a local user rotate their own password after verifying
// the current one. The env admin's password lives in the environment.
func (s *Service) ChangePassword(ctx context.Context, userID int64, current, next string) error {
	if userID == EnvAdminID {
		return fmt.Errorf("the environment admin's password is managed via KAODOKU_ADMIN_PASSWORD")
	}
	if len(next) < 4 {
		return fmt.Errorf("the new password must be at least 4 characters")
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash); err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		return fmt.Errorf("current password is incorrect")
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, string(newHash), userID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// SessionInfo describes one active session.
type SessionInfo struct {
	CreatedAt string
	ExpiresAt string
	Current   bool
}

// Sessions lists the user's active sessions; currentToken marks the caller's.
func (s *Service) Sessions(ctx context.Context, userID int64, currentToken string) ([]SessionInfo, error) {
	sum := sha256.Sum256([]byte(currentToken))
	cur := hex.EncodeToString(sum[:])
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash, created_at, expires_at FROM sessions WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionInfo
	for rows.Next() {
		var hash, created, expires string
		if rows.Scan(&hash, &created, &expires) != nil {
			continue
		}
		out = append(out, SessionInfo{CreatedAt: created, ExpiresAt: expires, Current: hash == cur})
	}
	return out, rows.Err()
}

// RevokeOtherSessions signs the user out everywhere except the current session.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID int64, currentToken string) error {
	sum := sha256.Sum256([]byte(currentToken))
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, hex.EncodeToString(sum[:]))
	return err
}

// APIToken is a named header token for scripted clients.
type APIToken struct {
	ID        int64
	Name      string
	CreatedAt string
	ExpiresAt string // "" = never expires
}

// DeleteAPITokensForDevice removes the tokens one app install minted, so a
// re-login replaces them.
func (s *Service) DeleteAPITokensForDevice(ctx context.Context, userID int64, deviceID string) error {
	if deviceID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ? AND device_id = ?`, userID, deviceID)
	return err
}

// CreateAPIToken mints a token (shown once); deviceID "" for manual tokens.
func (s *Service) CreateAPIToken(ctx context.Context, userID int64, name, deviceID string, ttlDays int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "token"
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "mgd_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := ""
	if ttlDays > 0 {
		expires = database.FormatTime(time.Now().AddDate(0, 0, ttlDays))
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens (token_hash, user_id, name, device_id, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hex.EncodeToString(sum[:]), userID, name, deviceID, expires); err != nil {
		return "", err
	}
	return token, nil
}

// APITokens lists a user's tokens (hashes never leave the DB).
func (s *Service) APITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, name, created_at, expires_at FROM api_tokens WHERE user_id = ? ORDER BY rowid`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.ExpiresAt) == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes one of the user's tokens.
func (s *Service) DeleteAPIToken(ctx context.Context, userID, tokenID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE rowid = ? AND user_id = ?`, tokenID, userID)
	return err
}

// PurgeExpiredAPITokens deletes expired tokens (empty expires_at never expires).
func (s *Service) PurgeExpiredAPITokens(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE expires_at != '' AND expires_at <= ?`, database.FormatTime(time.Now()))
	return err
}

// RevokeAPIToken deletes the token matching the given raw value (device sign-out).
func (s *Service) RevokeAPIToken(ctx context.Context, token string) error {
	sum := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE token_hash = ?`, hex.EncodeToString(sum[:]))
	return err
}

// UserByAPIToken resolves a header token to its user, or nil. Expired tokens do not resolve.
func (s *Service) UserByAPIToken(ctx context.Context, token string) (*User, error) {
	if !strings.HasPrefix(token, "mgd_") {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	var userID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM api_tokens WHERE token_hash = ? AND (expires_at = '' OR expires_at > ?)`,
		tokenHash, database.FormatTime(time.Now())).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.touchAPIToken(ctx, tokenHash)
	return s.GetUser(ctx, userID)
}

// touchAPIToken records token usage, throttled like session touches.
func (s *Service) touchAPIToken(ctx context.Context, tokenHash string) {
	if last, ok := s.touched.Load(tokenHash); ok && time.Since(last.(time.Time)) < time.Minute {
		return
	}
	s.touched.Store(tokenHash, time.Now())
	_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_seen_at = CURRENT_TIMESTAMP WHERE token_hash = ?`, tokenHash)
}

// ActiveDevice is an API-token client with recorded usage.
type ActiveDevice struct {
	TokenHash  string
	Username   string
	Name       string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// ActiveDevices lists unexpired API tokens with recorded usage, newest first.
func (s *Service) ActiveDevices(ctx context.Context) ([]ActiveDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.token_hash, u.username, t.name, t.created_at, t.last_seen_at
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.last_seen_at != '' AND (t.expires_at = '' OR t.expires_at > ?)
		ORDER BY t.last_seen_at DESC`, database.FormatTime(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveDevice
	for rows.Next() {
		var d ActiveDevice
		var created, seen string
		if err := rows.Scan(&d.TokenHash, &d.Username, &d.Name, &created, &seen); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = database.ParseTime(created)
		d.LastSeenAt, _ = database.ParseTime(seen)
		out = append(out, d)
	}
	return out, rows.Err()
}
