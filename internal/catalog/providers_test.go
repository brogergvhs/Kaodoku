package catalog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type recorded struct {
	method, path, query, body, auth, clientID string
}

func fakeAPI(t *testing.T, routes map[string]string) (*httptest.Server, *[]recorded) {
	t.Helper()
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Get("Authorization"), r.Header.Get("X-MAL-CLIENT-ID")})
		resp, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestMALSearchHeadersAndAdultFailClosed(t *testing.T) {
	srv, calls := fakeAPI(t, map[string]string{
		"GET /manga": `{"data":[
			{"node":{"id":1,"title":"Safe","media_type":"manhwa","nsfw":"white","status":"finished","num_chapters":40,"start_date":"2019-02-01","mean":8.25,"authors":[{"node":{"first_name":"A","last_name":"B"}}]}},
			{"node":{"id":2,"title":"Unknown"}}],"paging":{"next":"x"}}`,
	})
	c := NewMALClient(srv.Client())
	c.base = srv.URL
	c.SetClientID("cid")
	items, more, err := c.Search(context.Background(), "safe", 2, SearchFilter{Page: 2})
	if err != nil || !more || len(items) != 2 {
		t.Fatalf("search = %v %v %v", items, more, err)
	}
	m := items[0]
	if m.IsAdult || m.Format != "MANGA" || m.Status != "FINISHED" || *m.Chapters != 40 || m.Year != 2019 || m.AverageScore != 82 || m.Authors[0] != "A B" || !strings.Contains(m.RawJSON, "KR") {
		t.Fatalf("mapped manga = %+v", m)
	}
	if !items[1].IsAdult {
		t.Fatal("missing nsfw rating must count as adult")
	}
	q, _ := url.ParseQuery((*calls)[0].query)
	if (*calls)[0].clientID != "cid" || q.Get("offset") != "2" || q.Get("q") != "safe" {
		t.Fatalf("request = %+v", (*calls)[0])
	}
	if _, _, err := c.Search(WithAccount(context.Background(), Account{Token: "tok"}), "safe", 2, SearchFilter{}); err != nil {
		t.Fatal(err)
	}
	if last := (*calls)[1]; last.auth != "Bearer tok" || last.clientID != "" {
		t.Fatalf("authed request headers = %+v", last)
	}
}

func TestMALEntriesRoundTrip(t *testing.T) {
	srv, calls := fakeAPI(t, map[string]string{
		"GET /users/@me/mangalist":      `{"data":[{"node":{"id":5,"title":"A"},"list_status":{"status":"completed","num_chapters_read":12,"is_rereading":true}}],"paging":{}}`,
		"PATCH /manga/5/my_list_status": `{}`,
	})
	c := NewMALClient(srv.Client())
	c.base = srv.URL
	ctx := WithAccount(context.Background(), Account{Token: "t"})
	entries, err := c.ListEntries(ctx)
	if err != nil || len(entries) != 1 || entries[0].Status != "REPEATING" || entries[0].Progress != 12 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if err := c.SaveEntry(ctx, "5", EntryUpdate{Progress: 3, Status: "PLANNING", Started: time.Date(2024, 3, 1, 12, 0, 0, 0, time.Local)}); err != nil {
		t.Fatal(err)
	}
	form, _ := url.ParseQuery((*calls)[1].body)
	if form.Get("status") != "plan_to_read" || form.Get("num_chapters_read") != "3" || form.Get("start_date") != "2024-03-01" || form.Has("finish_date") {
		t.Fatalf("save form = %v", form)
	}
	if err := c.SaveEntry(ctx, "5", EntryUpdate{Progress: -1}); err != nil || len(*calls) != 2 {
		t.Fatalf("empty save should not call the API: %v %d", err, len(*calls))
	}
	if err := c.SaveEntry(ctx, "5", EntryUpdate{Progress: -1, Score: ScoreClear}); err != nil {
		t.Fatal(err)
	}
	if form, _ := url.ParseQuery((*calls)[2].body); len(form) != 1 || form.Get("score") != "0" {
		t.Fatalf("clearing the score sent %v", form)
	}
}

func TestKitsuGetMapsIncludedResources(t *testing.T) {
	srv, _ := fakeAPI(t, map[string]string{
		"GET /manga/9": `{"data":{"id":"9","type":"manga","attributes":{"canonicalTitle":"Nana","titles":{"en_jp":"Nana","ja_jp":"ナナ"},"subtype":"manga","status":"finished","ageRating":"PG","chapterCount":84,"posterImage":{"large":"p.jpg"},"coverImage":{"large":"b.jpg"}},
			"relationships":{"categories":{"data":[{"type":"categories","id":"c1"}]},"mappings":{"data":[{"type":"mappings","id":"m1"},{"type":"mappings","id":"m2"}]},"mediaRelationships":{"data":[{"type":"mediaRelationships","id":"r1"}]}}},
			"included":[
				{"id":"c1","type":"categories","attributes":{"title":"Drama"}},
				{"id":"m1","type":"mappings","attributes":{"externalSite":"myanimelist/manga","externalId":"28"}},
				{"id":"m2","type":"mappings","attributes":{"externalSite":"anilist/manga","externalId":"30028"}},
				{"id":"r1","type":"mediaRelationships","attributes":{"role":"sequel"},"relationships":{"destination":{"data":{"type":"manga","id":"10"}}}},
				{"id":"10","type":"manga","attributes":{"canonicalTitle":"Nana 2"}}]}`,
	})
	c := NewKitsuClient(srv.Client())
	c.base = srv.URL
	m, rels, err := c.Get(context.Background(), "9")
	if err != nil {
		t.Fatal(err)
	}
	if m.IsAdult || m.Genres[0] != "Drama" || m.ExternalIDs[MALProvider] != "28" || m.ExternalIDs[AniListProvider] != "30028" || m.BannerImage != "b.jpg" || m.TitleNative != "ナナ" {
		t.Fatalf("manga = %+v", m)
	}
	if len(rels) != 1 || rels[0].ProviderID != "10" || rels[0].Type != "SEQUEL" {
		t.Fatalf("relations = %+v", rels)
	}
	related, _, _ := c.Related(context.Background(), "9", 5)
	if len(related) != 1 || !related[0].IsAdult {
		t.Fatalf("unrated related work must count as adult: %+v", related)
	}
}

func TestKitsuSaveCreatesThenPatches(t *testing.T) {
	srv, calls := fakeAPI(t, map[string]string{
		"GET /library-entries":      `{"data":[]}`,
		"POST /library-entries":     `{}`,
		"PATCH /library-entries/77": `{}`,
	})
	c := NewKitsuClient(srv.Client())
	c.base = srv.URL
	ctx := WithAccount(context.Background(), Account{Token: "t", UserID: "42"})
	if err := c.SaveEntry(ctx, "9", EntryUpdate{Progress: 4, Status: "CURRENT", Finished: time.Date(2024, 3, 9, 10, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	post := (*calls)[1]
	if post.method != http.MethodPost || !strings.Contains(post.body, `"finishedAt":"2024-03-09T10:00:00Z"`) || !strings.Contains(post.body, `"status":"current"`) || !strings.Contains(post.body, `"id":"42"`) || post.auth != "Bearer t" {
		t.Fatalf("create = %+v", post)
	}
	q, _ := url.ParseQuery((*calls)[0].query)
	if q.Get("filter[userId]") != "42" || q.Get("filter[mangaId]") != "9" {
		t.Fatalf("lookup query = %v", q)
	}
	if err := c.SaveEntry(ctx, "9", EntryUpdate{Progress: 5, Score: 7, Remote: &Entry{Ref: "77"}}); err != nil || len(*calls) != 3 || (*calls)[2].path != "/library-entries/77" || !strings.Contains((*calls)[2].body, `"ratingTwenty":14`) {
		t.Fatalf("patch with a known entry = %+v, %v", (*calls)[2:], err)
	}
}

func TestMangaUpdatesListsAndSave(t *testing.T) {
	srv, calls := fakeAPI(t, map[string]string{
		"POST /series/search":       `{"total_hits":3,"results":[{"record":{"series_id":55,"title":"Solo","type":"Manhwa","year":"2018","genres":[{"genre":"Action"}]}},{"record":{"series_id":56,"title":"X","genres":[{"genre":"Smut"}]}}]}`,
		"GET /lists/series/55":      `{"series":{"id":55},"list_id":4,"status":{"chapter":7}}`,
		"POST /lists/series/update": `{}`,
		"POST /lists/series":        `{}`,
		"PUT /series/55/rating":     `{}`,
	})
	c := NewMangaUpdatesClient(srv.Client())
	c.base = srv.URL
	items, more, err := c.Search(context.Background(), "solo", 2, SearchFilter{})
	if err != nil || !more || items[0].IsAdult || items[0].Year != 2018 || !items[1].IsAdult {
		t.Fatalf("search = %+v %v %v", items, more, err)
	}
	ctx := WithAccount(context.Background(), Account{Token: "s"})
	e, ok, err := c.GetEntry(ctx, "55")
	if err != nil || !ok || e.Status != "PAUSED" || e.Progress != 7 {
		t.Fatalf("entry = %+v %v %v", e, ok, err)
	}
	n := len(*calls)
	if err := c.SaveEntry(ctx, "55", EntryUpdate{Progress: 9, Remote: &e}); err != nil || len(*calls) != n+1 {
		t.Fatalf("save with the entry just read took %d requests, %v", len(*calls)-n, err)
	}
	var update []map[string]any
	_ = json.Unmarshal([]byte((*calls)[len(*calls)-1].body), &update)
	if (*calls)[len(*calls)-1].path != "/lists/series/update" || update[0]["list_id"] != float64(4) || update[0]["status"].(map[string]any)["chapter"] != float64(9) {
		t.Fatalf("update = %+v", update)
	}
	if err := c.SaveEntry(ctx, "55", EntryUpdate{Progress: -1, Score: 8, Remote: &e}); err != nil || len(*calls) != n+2 || (*calls)[n+1].path != "/series/55/rating" || (*calls)[n+1].body != `{"rating":8}` {
		t.Fatalf("a rating alone should be one rating request: %+v, %v", (*calls)[n+1:], err)
	}
	if err := c.SaveEntry(ctx, "77", EntryUpdate{Status: "PLANNING"}); err != nil {
		t.Fatal(err)
	}
	if last := (*calls)[len(*calls)-1]; last.path != "/lists/series" || !strings.Contains(last.body, `"list_id":1`) {
		t.Fatalf("add = %+v", last)
	}
}

func TestMatchScoreThresholds(t *testing.T) {
	base := Manga{TitleRomaji: "Shingeki no Kyojin", TitleEnglish: "Attack on Titan", Year: 2009, Format: "MANGA"}
	exact := Manga{TitleRomaji: "Attack on Titan!", Year: 2009, Format: "MANGA"}
	if s := MatchScore(base, exact); s < 0.92 {
		t.Fatalf("exact title+year+format = %.2f, want auto-confirm", s)
	}
	if s := MatchScore(base, Manga{TitleRomaji: "Attack on Titan", Format: "MANGA"}); s >= 0.92 || s < 0.75 {
		t.Fatalf("title without year = %.2f, want suggestion band", s)
	}
	if s := MatchScore(base, Manga{TitleRomaji: "Attack on Titan: Before the Fall", Year: 2011, Format: "NOVEL"}); s >= 0.75 {
		t.Fatalf("spin-off novel = %.2f, want no match", s)
	}
}

func TestMangaUpdatesRelatedIsOneRequest(t *testing.T) {
	srv, calls := fakeAPI(t, map[string]string{
		"GET /series/1": `{"series_id":1,"title":"Main","genres":[{"genre":"Drama"}],"related_series":[{"relation_type":"Side Story","related_series_id":2,"related_series_name":"Side"},{"relation_type":"Sequel","related_series_id":3,"related_series_name":"Gone"}]}`,
	})
	c := NewMangaUpdatesClient(srv.Client())
	c.base = srv.URL
	rels, _, err := c.Related(context.Background(), "1", 5)
	if err != nil || len(rels) != 2 || len(*calls) != 1 {
		t.Fatalf("related = %+v, %v after %d requests", rels, err, len(*calls))
	}
	if rels[0].TitleRomaji != "Side" || !rels[0].IsAdult || rels[0].RelationType != "SIDE_STORY" || rels[1].RelationType != "SEQUEL" {
		t.Fatalf("relations = %+v", rels)
	}
}

func TestMALAuthorizeOmitsRedirect(t *testing.T) {
	u, _ := url.Parse(NewMALClient(nil).AuthorizeURL(App{ClientID: "id", RedirectURI: "http://wrong/cb"}, "st", "v"))
	if q := u.Query(); q.Has("redirect_uri") || q.Get("code_challenge") != "v" || q.Get("state") != "st" {
		t.Fatalf("authorize url = %s", u)
	}
}

func TestKitsuClampsProgressToChapterCount(t *testing.T) {
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/library-entries":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/manga/9":
			_, _ = w.Write([]byte(`{"data":{"id":"9","type":"manga","attributes":{"chapterCount":40}}}`))
		case r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			posts = append(posts, string(body))
			if strings.Contains(string(body), `"progress":55`) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"errors":[{"detail":"progress - cannot exceed length of media"}]}`))
			}
		}
	}))
	defer srv.Close()
	c := NewKitsuClient(srv.Client())
	c.base = srv.URL
	if err := c.SaveEntry(WithAccount(context.Background(), Account{Token: "t", UserID: "1"}), "9", EntryUpdate{Progress: 55, Status: "CURRENT"}); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || !strings.Contains(posts[1], `"progress":40`) {
		t.Fatalf("posts = %v, want a retry clamped to 40", posts)
	}
}

func TestMangaUpdatesRetriesOperationLimit(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"status":"exception","reason":"That operation is limited to 1 time per 1 seconds. Please wait 1 seconds then try again."}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewMangaUpdatesClient(srv.Client())
	c.base = srv.URL
	if err := c.DeleteEntry(context.Background(), "5"); err != nil || hits != 2 {
		t.Fatalf("delete = %v after %d hits, want retried success", err, hits)
	}
}

func TestAniListEntryDates(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"data":{"Media":{"mediaListEntry":{"progress":3,"status":"CURRENT","score":75,"startedAt":{"year":2024,"month":3,"day":1},"completedAt":{"year":null,"month":null,"day":null}}}}}`))
	}))
	defer srv.Close()
	c := NewAniListClient(srv.Client())
	c.endpoint = srv.URL
	e, ok, err := c.GetEntry(context.Background(), "1")
	if err != nil || !ok || e.Started.Format(time.DateOnly) != "2024-03-01" || !e.Finished.IsZero() || e.Score != 8 {
		t.Fatalf("entry = %+v %v %v", e, ok, err)
	}
	if err := c.SaveEntry(context.Background(), "1", EntryUpdate{Progress: -1, Score: 7, Finished: time.Date(2024, 3, 9, 12, 0, 0, 0, time.Local)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"completedAt":{"day":9,"month":3,"year":2024}`) || !strings.Contains(body, `"scoreRaw":70`) || strings.Contains(body, `"progress"`) {
		t.Fatalf("save vars = %s", body)
	}
}
