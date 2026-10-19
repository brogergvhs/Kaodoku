package library

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/providers"
)

func TestPageTitlesFiltersSortsAndPagesInSQL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "kaodoku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sources(id, name) VALUES ('mangadex', 'MangaDex'), ('other', 'Other')`)
	repo := NewRepository(db)
	add := func(name, source, url string, monitored bool, tags, genres string, score, adult int, chapterCount, downloaded int) int64 {
		t.Helper()
		title, err := repo.AddTitle(ctx, AddTitleParams{SourceID: source, SourceURL: url, DisplayTitle: name, Monitored: monitored})
		if err != nil {
			t.Fatal(err)
		}
		if tags != "" || genres != "" {
			exec(`INSERT INTO catalog_manga(provider, provider_id, tags_json, genres_json, average_score, is_adult) VALUES ('anilist', ?, ?, ?, ?, ?)`, name, tags, genres, score, adult)
			exec(`UPDATE titles SET catalog_manga_id = (SELECT id FROM catalog_manga WHERE provider_id = ?) WHERE id = ?`, name, title.ID)
		}
		var list []chapters.Chapter
		for i := 1; i <= chapterCount; i++ {
			list = append(list, chapters.Chapter{Chapter: providers.Chapter{URL: fmt.Sprintf("%s/%d", url, i), Label: fmt.Sprint(i), NumMain: i}})
		}
		if _, err := repo.UpsertChapters(ctx, title.ID, list); err != nil {
			t.Fatal(err)
		}
		chs, _ := repo.ListChapters(ctx, title.ID)
		for i := 0; i < downloaded; i++ {
			if err := repo.MarkDownloadCompleted(ctx, chs[i].ID, "/x.cbz", 10, 2); err != nil {
				t.Fatal(err)
			}
		}
		return title.ID
	}
	alpha := add("Alpha", "mangadex", "https://mangadex.org/a", true, `["Action"]`, `["Drama"]`, 71, 0, 2, 2)
	master := add("Мастер", "other", "https://other.to/b", false, `[]`, `["Horror"]`, 0, 0, 2, 1)
	beta := add("beta", "", "local:import", true, "", "", 0, 0, 0, 0)
	gamma := add("Gamma", "", "https://x.test/c", true, `[" ACTION "]`, `[]`, 88, 1, 0, 0)
	exec(`INSERT INTO title_sources(title_id, source_id, url) VALUES (?, 'mangadex', 'https://mangadex.org/beta')`, beta)
	exec(`INSERT INTO volumes(title_id, file, bytes) VALUES (?, '/v1.cbz', 500)`, beta)
	exec(`INSERT INTO user_favourites(user_id, title_id) VALUES (?, ?)`, auth.UserID(ctx), master)

	ids := func(q TitleQuery) []int64 {
		t.Helper()
		got, err := repo.pageIDs(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			got = []int64{}
		}
		return got
	}
	for name, tc := range map[string]struct {
		q    TitleQuery
		want []int64
	}{
		"default order":         {TitleQuery{}, []int64{alpha, beta, gamma, master}},
		"include tags":          {TitleQuery{IncludeAny: [][]string{{"action", "horror"}}}, []int64{alpha, gamma, master}},
		"exclude tags":          {TitleQuery{Exclude: []string{"horror"}}, []int64{alpha, beta, gamma}},
		"include and exclude":   {TitleQuery{IncludeAny: [][]string{{"action", "horror"}}, Exclude: []string{"drama"}}, []int64{gamma, master}},
		"every include group":   {TitleQuery{IncludeAny: [][]string{{"action"}, {"drama"}}}, []int64{alpha}},
		"unicode search":        {TitleQuery{Search: "мастер"}, []int64{master}},
		"search source id":      {TitleQuery{Search: "MANGADEX"}, []int64{alpha}},
		"own or linked source":  {TitleQuery{Sources: []string{"mangadex"}}, []int64{alpha, beta}},
		"linked urls":           {TitleQuery{Sources: []string{"linked"}}, []int64{alpha, gamma, master}},
		"imported":              {TitleQuery{Sources: []string{"imported"}}, []int64{beta}},
		"any of sources":        {TitleQuery{Sources: []string{"mangadex", "other"}}, []int64{alpha, beta, master}},
		"missing":               {TitleQuery{Progress: "missing"}, []int64{master}},
		"complete":              {TitleQuery{Progress: "complete"}, []int64{alpha}},
		"empty":                 {TitleQuery{Progress: "empty"}, []int64{beta, gamma}},
		"has volumes":           {TitleQuery{Content: "volumes"}, []int64{beta}},
		"unmonitored":           {TitleQuery{Monitored: "off"}, []int64{master}},
		"favourites":            {TitleQuery{FavouriteOnly: true}, []int64{master}},
		"no adult":              {TitleQuery{NoAdult: true}, []int64{alpha, beta, master}},
		"adult only":            {TitleQuery{AdultOnly: true}, []int64{gamma}},
		"rating desc":           {TitleQuery{Sort: "rating", Dir: "desc"}, []int64{gamma, alpha, beta, master}},
		"rating asc":            {TitleQuery{Sort: "rating"}, []int64{beta, master, alpha, gamma}},
		"missing desc":          {TitleQuery{Sort: "missing", Dir: "desc"}, []int64{master, alpha, beta, gamma}},
		"size desc":             {TitleQuery{Sort: "size", Dir: "desc"}, []int64{beta, alpha, master, gamma}},
		"title desc":            {TitleQuery{Sort: "title", Dir: "desc"}, []int64{master, gamma, beta, alpha}},
		"page":                  {TitleQuery{Offset: 1, Limit: 2}, []int64{beta, gamma}},
		"ids":                   {TitleQuery{IDs: []int64{gamma, alpha}}, []int64{alpha, gamma}},
		"empty ids match none":  {TitleQuery{IDs: []int64{}}, []int64{}},
		"filters combine (AND)": {TitleQuery{Sources: []string{"linked"}, Monitored: "on"}, []int64{alpha, gamma}},
	} {
		if got := ids(tc.q); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ids = %v, want %v", name, got, tc.want)
		}
	}

	if n, err := repo.CountTitles(ctx, TitleQuery{NoAdult: true, Limit: 1}); err != nil || n != 3 {
		t.Fatalf("CountTitles = %d, %v; want 3", n, err)
	}
	page, err := repo.PageTitles(ctx, TitleQuery{Sort: "missing", Dir: "desc", Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID != master || page[1].ID != alpha {
		t.Fatalf("PageTitles = %+v, %v", page, err)
	}
	if page[0].DiscoveredCount != 2 || page[0].MissingCount != 1 || !page[0].Favourite || page[1].MissingCount != 0 || page[1].SizeBytes != 20 {
		t.Fatalf("page aggregates = %+v / %+v", page[0], page[1])
	}
	exec(`INSERT INTO chapter_read_progress(user_id, chapter_id, completed, last_read_at) SELECT ?, id, 1, '2026-01-0' || id || ' 00:00:00' FROM chapters WHERE title_id IN (?, ?)`, auth.UserID(ctx), alpha, master)
	exec(`UPDATE chapter_read_progress SET completed = 0 WHERE chapter_id = (SELECT MAX(id) FROM chapters WHERE title_id = ?)`, master)
	if genre, err := repo.TopGenre(ctx, TitleQuery{}); err != nil || genre != "Drama" {
		t.Fatalf("TopGenre = %q, %v; want Drama", genre, err)
	}
	if genre, err := repo.TopGenre(ctx, TitleQuery{Exclude: []string{"drama"}}); err != nil || genre != "Horror" {
		t.Fatalf("TopGenre without drama = %q, %v; want Horror", genre, err)
	}
	read, more, err := repo.LastReadAt(ctx, 1)
	if err != nil || len(read) != 1 || !more || read[master].IsZero() {
		t.Fatalf("LastReadAt(1) = %v, %v, %v; want only the newest title", read, more, err)
	}
	if read, more, err = repo.LastReadAt(ctx, 5); err != nil || len(read) != 2 || more {
		t.Fatalf("LastReadAt(5) = %v, %v, %v", read, more, err)
	}
	exec(`UPDATE downloads SET completed_at = '2026-02-0' || chapter_id || ' 00:00:00'`)
	exec(`UPDATE volumes SET created_at = '2025-01-01 00:00:00'`)
	arrivals, more, err := repo.LatestArrivals(ctx, 1)
	if err != nil || len(arrivals) != 2 || !more || arrivals[master].Label != "Ch 1" || arrivals[alpha].Label != "" {
		t.Fatalf("LatestArrivals(1) = %v, %v, %v", arrivals, more, err)
	}
	if arrivals, more, err = repo.LatestArrivals(ctx, 5); err != nil || len(arrivals) != 3 || more || arrivals[beta].Label != "New volume" || arrivals[alpha].Label != "Ch 2" {
		t.Fatalf("LatestArrivals(5) = %v, %v, %v", arrivals, more, err)
	}

	got, err := repo.GetTitle(ctx, beta)
	if err != nil || got.VolumeCount != 1 || got.VolumeBytes != 500 || got.DiscoveredCount != 0 {
		t.Fatalf("GetTitle(beta) = %+v, %v", got, err)
	}
}
