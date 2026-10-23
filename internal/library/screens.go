package library

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/brogergvhs/kaodoku/internal/auth"
)

// ScreenConfig is a saved library view: hard content constraints plus
// defaults for the ad-hoc filter controls.
type ScreenConfig struct {
	IncludeTags []string `json:"include_tags,omitempty"`
	ExcludeTags []string `json:"exclude_tags,omitempty"`
	Adult       string   `json:"adult,omitempty"` // "", "only", "exclude"
	Monitor     string   `json:"monitor,omitempty"`
	Fav         string   `json:"fav,omitempty"`
	Source      string   `json:"source,omitempty"`
	Progress    string   `json:"progress,omitempty"`
	Content     string   `json:"content,omitempty"`
	Sort        string   `json:"sort,omitempty"`
	Dir         string   `json:"dir,omitempty"`
	View        string   `json:"view,omitempty"`
}

// Screen is a user's saved library view.
type Screen struct {
	ID     int64
	UserID int64
	Name   string
	Config ScreenConfig
}

// Matches applies the screen's hard content constraints to a title.
func (c ScreenConfig) Matches(t Title) bool {
	switch c.Adult {
	case "only":
		if !t.IsAdult {
			return false
		}
	case "exclude":
		if t.IsAdult {
			return false
		}
	}
	if len(c.IncludeTags) > 0 && !HasAnyTag(t.ContentTags, c.IncludeTags) {
		return false
	}
	if len(c.ExcludeTags) > 0 && HasAnyTag(t.ContentTags, c.ExcludeTags) {
		return false
	}
	return true
}

// HasAnyTag reports whether any wanted tag is present (case-insensitive).
// HasAnyTag matches names ignoring case, spaces and punctuation, so "Sci Fi" is "Sci-Fi".
func HasAnyTag(have, want []string) bool {
	for _, w := range want {
		for _, h := range have {
			if TagKey(w) == TagKey(h) {
				return true
			}
		}
	}
	return false
}

func TagKey(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, name)
}

// Screens lists the acting user's saved views.
func (r *Repository) Screens(ctx context.Context) ([]Screen, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, user_id, name, config_json FROM library_screens WHERE user_id = ? ORDER BY position, id`, auth.UserID(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Screen
	for rows.Next() {
		var sc Screen
		var cfg string
		if err := rows.Scan(&sc.ID, &sc.UserID, &sc.Name, &cfg); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(cfg), &sc.Config)
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetScreen returns one of the acting user's screens.
func (r *Repository) GetScreen(ctx context.Context, id int64) (Screen, error) {
	var sc Screen
	var cfg string
	err := r.db.QueryRowContext(ctx, `SELECT id, user_id, name, config_json FROM library_screens WHERE id = ? AND user_id = ?`, id, auth.UserID(ctx)).
		Scan(&sc.ID, &sc.UserID, &sc.Name, &cfg)
	if err != nil {
		return Screen{}, fmt.Errorf("screen not found")
	}
	_ = json.Unmarshal([]byte(cfg), &sc.Config)
	return sc, nil
}

// SaveScreen creates (id 0) or updates one of the acting user's screens.
func (r *Repository) SaveScreen(ctx context.Context, sc Screen) (int64, error) {
	sc.Name = strings.TrimSpace(sc.Name)
	if sc.Name == "" {
		return 0, fmt.Errorf("a screen name is required")
	}
	cfg, err := json.Marshal(sc.Config)
	if err != nil {
		return 0, err
	}
	if sc.ID > 0 {
		_, err := r.db.ExecContext(ctx, `UPDATE library_screens SET name = ?, config_json = ? WHERE id = ? AND user_id = ?`, sc.Name, string(cfg), sc.ID, auth.UserID(ctx))
		return sc.ID, err
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO library_screens (user_id, name, config_json, position)
		VALUES (?, ?, ?, COALESCE((SELECT MAX(position)+1 FROM library_screens WHERE user_id = ?), 0))`,
		auth.UserID(ctx), sc.Name, string(cfg), auth.UserID(ctx))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ReorderScreens sets positions from the given id order, ignoring ids that
// aren't the acting user's.
func (r *Repository) ReorderScreens(ctx context.Context, ids []int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for pos, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE library_screens SET position = ? WHERE id = ? AND user_id = ?`, pos, id, auth.UserID(ctx)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteScreen removes one of the acting user's screens.
func (r *Repository) DeleteScreen(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM library_screens WHERE id = ? AND user_id = ?`, id, auth.UserID(ctx))
	return err
}
