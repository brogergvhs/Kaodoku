package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
)

type unauthorizedAniList struct{}

func (unauthorizedAniList) Search(context.Context, string, int, catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	return nil, false, nil
}
func (unauthorizedAniList) Get(context.Context, int) (catalog.Manga, error) {
	return catalog.Manga{}, nil
}
func (unauthorizedAniList) TagVocabulary(context.Context) ([]string, []catalog.ContentTag, error) {
	return nil, nil, nil
}
func (unauthorizedAniList) Related(context.Context, int, int) ([]catalog.Manga, []catalog.Manga, error) {
	return nil, nil, nil
}
func (unauthorizedAniList) Trending(context.Context, int) ([]catalog.Manga, error) {
	return nil, nil
}
func (unauthorizedAniList) GetWithRelations(context.Context, int) (catalog.Manga, []catalog.Relation, error) {
	return catalog.Manga{}, nil, nil
}
func (unauthorizedAniList) UserList(context.Context, int) ([]catalog.AniListEntry, error) {
	return nil, catalog.ErrAniListUnauthorized
}
func (unauthorizedAniList) MediaEntry(context.Context, int) (int, string, bool, error) {
	return 0, "", false, nil
}
func (unauthorizedAniList) SaveEntry(context.Context, int, int, string) error {
	return nil
}
func (unauthorizedAniList) DeleteEntry(context.Context, int, int) error {
	return nil
}
func (unauthorizedAniList) FavouriteManga(context.Context, int) ([]int, error) {
	return nil, nil
}
func (unauthorizedAniList) IsFavourite(context.Context, int) (bool, error) {
	return false, nil
}
func (unauthorizedAniList) ToggleFavourite(context.Context, int) error {
	return nil
}

func TestAniListUnauthorizedDisconnects(t *testing.T) {
	ctx := context.Background()
	svc, closeDB, err := OpenJobs(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	token, err := svc.secrets.Encrypt("bad-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO user_anilist (user_id, access_token, anilist_user_id, anilist_name) VALUES (1, ?, 42, 'u')`, token); err != nil {
		t.Fatal(err)
	}
	svc.want.anilist = unauthorizedAniList{}

	err = svc.runAniListSync(ctx, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "reconnect AniList") {
		t.Fatalf("sync error = %v", err)
	}
	if conn := svc.AniListConnectionFor(ctx, 1); conn.Connected {
		t.Fatalf("connection still present: %#v", conn)
	}
}
