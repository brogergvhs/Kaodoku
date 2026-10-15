package kotatsu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testServer(t *testing.T) (*Client, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.Header.Get("X-Worker-Key") != "k" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "k"), mux
}

func TestClientRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, mux := testServer(t)

	mux.HandleFunc("/sources", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"MANGADEX","name":"MangaDex","language":"en",
			"content_type":"manga","nsfw":false,"broken":false,
			"domains":["mangadex.org"],"base_url":"https://mangadex.org"}]`))
	})
	mux.HandleFunc("/chapters", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("source") != "MANGADEX" || r.URL.Query().Get("url") != "https://m/x" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"resolved_url":"https://m/x","chapters":
			[{"url":"c1","number":"12.5","title":"T","volume":3,"branch":"English","uploaded_at":""}]}`))
	})
	mux.HandleFunc("/pages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("manga") != "https://m/x" || r.URL.Query().Get("chapter") != "c1" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[{"image":"/image?source=S&url=u&sig=s"}]`))
	})

	srcs, err := c.Sources(ctx)
	if err != nil || len(srcs) != 1 || srcs[0].ID != "MANGADEX" || srcs[0].Domains[0] != "mangadex.org" {
		t.Fatalf("Sources = %+v, %v", srcs, err)
	}
	chs, err := c.Chapters(ctx, "MANGADEX", "https://m/x")
	if err != nil || chs.ResolvedURL != "https://m/x" || len(chs.Chapters) != 1 ||
		chs.Chapters[0].Number != "12.5" || chs.Chapters[0].Volume != 3 {
		t.Fatalf("Chapters = %+v, %v", chs, err)
	}
	pages, err := c.Pages(ctx, "MANGADEX", "https://m/x", "c1")
	if err != nil || len(pages) != 1 || pages[0] != c.endpoint+"/image?source=S&url=u&sig=s" {
		t.Fatalf("Pages = %v, %v", pages, err)
	}
}

func TestClientErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, mux := testServer(t)
	mux.HandleFunc("/search", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"unknown source"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"parsers":3,"version":"v"}`))
	})

	if _, err := c.Search(ctx, "NOPE", "q"); err == nil {
		t.Fatal("want error for 404")
	}
	bad := New(c.endpoint, "wrong")
	if _, err := bad.Sources(ctx); err == nil {
		t.Fatal("want error for bad key")
	}
	if h, err := bad.Health(ctx); err != nil || !h.OK || h.Version != "v" {
		t.Fatalf("Health = %+v, %v (healthz must work without a key)", h, err)
	}
}
