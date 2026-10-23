package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestMangaBakaTracksWithAnAccessToken(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var saved map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("x-api-key") != "mb-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/my/library" && r.URL.Query().Get("page") == "1":
			_, _ = w.Write([]byte(`{"data":[{"series_id":34798,"state":"rereading","rating":85,"progress_chapter":12,"start_date":"2026-01-02"}],"pagination":{"next":"/my/library?page=2"}}`))
		case r.URL.Path == "/my/library":
			_, _ = w.Write([]byte(`{"data":[{"series":{"id":2553,"title":"Made in Abyss"},"state":"considering"}],"pagination":{"next":null}}`))
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&saved)
			_, _ = w.Write([]byte(`{"status":201,"data":true}`))
		}
	}))
	defer srv.Close()
	c := NewMangaBakaClient(nil, nil)
	c.base = srv.URL
	ctx := WithAccount(context.Background(), Account{Token: "mb-tok"})

	entries, err := c.ListEntries(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if e := entries[0]; e.Manga.ProviderID != "34798" || e.Status != "REPEATING" || e.Score != 9 || e.Progress != 12 || e.Started.Day() != 2 {
		t.Fatalf("first entry = %+v", e)
	}
	if e := entries[1]; e.Manga.ProviderID != "2553" || e.Status != "PLANNING" {
		t.Fatalf("second entry = %+v", e)
	}
	if err := c.SaveEntry(ctx, "2553", EntryUpdate{Progress: 3, Score: 7}); err != nil {
		t.Fatal(err)
	}
	if saved["state"] != "reading" || saved["progress_chapter"] != float64(3) || saved["rating"] != float64(70) {
		t.Fatalf("created entry = %v after %v", saved, calls)
	}
}
