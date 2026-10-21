package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderVocabulariesAndFilters(t *testing.T) {
	ctx := context.Background()

	mu, calls := fakeAPI(t, map[string]string{
		"GET /genres":         `[{"genre":"Action","description":"Fights"},{"genre":"Smut"}]`,
		"POST /series/search": `{"total_hits":0,"results":[]}`,
	})
	m := NewMangaUpdatesClient(mu.Client())
	m.base = mu.URL
	tags, err := m.TagVocabulary(ctx)
	if err != nil || len(tags) != 2 || tags[0].Kind != "genre" || tags[0].Description != "Fights" {
		t.Fatalf("mangaupdates vocabulary = %+v, %v", tags, err)
	}
	if _, _, err := m.Search(ctx, "", 5, SearchFilter{GenreIn: []string{"Horror"}, TagIn: []string{"Drama"}, GenreNotIn: []string{"Comedy"}}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte((*calls)[len(*calls)-1].body), &body)
	if g, _ := json.Marshal(body["genre"]); string(g) != `["Horror","Drama"]` {
		t.Fatalf("genre filter = %s", g)
	}
	if g, _ := json.Marshal(body["exclude_genre"]); string(g) != `["Comedy"]` {
		t.Fatalf("exclude filter = %s", g)
	}

	var queries []string
	kitsu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Query().Get("page[offset]") + r.URL.Path {
		case "0/categories":
			_, _ = w.Write([]byte(`{"data":[{"id":"1","type":"categories","attributes":{"title":"Horror","slug":"horror"}}],"links":{"next":"more"}}`))
		case "20/categories":
			_, _ = w.Write([]byte(`{"data":[{"id":"2","type":"categories","attributes":{"title":"Nudity","slug":"nudity","nsfw":true}}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"9","type":"manga","attributes":{"canonicalTitle":"X","ageRating":"PG"},"relationships":{"categories":{"data":[{"type":"categories","id":"2"}]}}}],
				"included":[{"id":"2","type":"categories","attributes":{"title":"Nudity","nsfw":true}}]}`))
		}
	}))
	defer kitsu.Close()
	k := NewKitsuClient(kitsu.Client())
	k.base = kitsu.URL
	tags, err = k.TagVocabulary(ctx)
	if err != nil || len(tags) != 2 || tags[0].Slug != "horror" || tags[0].IsAdult || !tags[1].IsAdult {
		t.Fatalf("kitsu vocabulary = %+v, %v", tags, err)
	}
	items, _, err := k.Search(ctx, "", 5, SearchFilter{GenreIn: []string{"horror"}, TagIn: []string{"space"}})
	if err != nil || len(items) != 1 || !items[0].IsAdult {
		t.Fatalf("an nsfw category must make the manga adult: %+v %v", items, err)
	}
	if q := queries[len(queries)-1]; !strings.Contains(q, "filter%5Bcategories%5D=horror%2Cspace") {
		t.Fatalf("kitsu filter missing: %s", q)
	}

	mal, _ := NewMALClient(nil).TagVocabulary(ctx)
	adult := 0
	for _, tag := range mal {
		if AdultName(tag.Name) {
			adult++
		}
	}
	if len(mal) < 70 || adult != 2 {
		t.Fatalf("mal vocabulary = %d tags, %d adult", len(mal), adult)
	}
}
