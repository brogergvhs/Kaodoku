package kotatsu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func scraperServer(t *testing.T) *Scraper {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chapters":
			_, _ = w.Write([]byte(`{"resolved_url":"https://m/x2","chapters":[
				{"url":"c1","number":"1","title":"One","volume":1,"branch":"Français","uploaded_at":""},
				{"id":42,"url":"c1en","number":"1","title":"One EN","volume":1,"branch":"English","uploaded_at":""},
				{"url":"c2","number":"2.5","title":"Half","volume":1,"branch":"English","uploaded_at":""},
				{"url":"c3","number":"","title":"Oneshot","volume":0,"branch":"English","uploaded_at":""}]}`))
		case "/pages":
			if r.URL.Query().Get("manga") != "https://m/x2" || r.URL.Query().Get("chapter") != "c1en" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[{"image":"/image?a=1"},{"image":"/image?a=2"}]`))
		case "/search":
			_, _ = w.Write([]byte(`[{"url":"https://m/a","title":"A","cover":"","alt_titles":[]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewScraper(New(srv.URL, "k"), "MANGADEX")
}

func TestScraperChapters(t *testing.T) {
	t.Parallel()
	s := scraperServer(t)
	chs, resolved, err := s.GetChaptersResolved(context.Background(), "https://m/x")
	if err != nil || resolved != "https://m/x2" {
		t.Fatalf("resolved = %q, %v", resolved, err)
	}
	if len(chs) != 2 {
		t.Fatalf("chapters = %+v", chs)
	}
	if chs[0].Label != "1" || chs[0].Title != "One EN" || chs[0].URL != "https://m/x2\tc1en" {
		t.Fatalf("ch[0] = %+v", chs[0])
	}
	if chs[0].Volume != 1 || chs[0].UID != "42" {
		t.Fatalf("ch[0] volume/uid = %v %q", chs[0].Volume, chs[0].UID)
	}
	if chs[1].Label != "2.5" || chs[1].NumMain != 2 || chs[1].SuffixNum != 5 {
		t.Fatalf("ch[1] = %+v", chs[1])
	}
}

func TestScraperNumbersGalleryChapters(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resolved_url":"https://g/390009","chapters":[
			{"url":"/g/390009/","number":"","title":"Chapter","volume":0,"branch":"","uploaded_at":""}]}`))
	}))
	defer srv.Close()

	chs, _, err := NewScraper(New(srv.URL, "k"), "NHENTAI").GetChaptersResolved(context.Background(), "https://g/390009")
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 1 {
		t.Fatalf("a source that numbers nothing must keep its chapters: %+v", chs)
	}
	if chs[0].Label != "1" || chs[0].URL != "https://g/390009\t/g/390009/" {
		t.Fatalf("ch[0] = %+v", chs[0])
	}
}

func TestScraperNumbersUnnumberedPerBranch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resolved_url":"https://m/x","chapters":[
			{"url":"f1","number":"","title":"Un","volume":0,"branch":"Français","uploaded_at":""},
			{"url":"f2","number":"","title":"Deux","volume":0,"branch":"Français","uploaded_at":""},
			{"url":"e1","number":"","title":"One","volume":0,"branch":"English","uploaded_at":""},
			{"url":"e2","number":"","title":"Two","volume":0,"branch":"English","uploaded_at":""}]}`))
	}))
	defer srv.Close()

	chs, _, err := NewScraper(New(srv.URL, "k"), "SRC").GetChaptersResolved(context.Background(), "https://m/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 2 {
		t.Fatalf("branches must collapse, got %+v", chs)
	}
	if chs[0].Label != "1" || chs[0].URL != "https://m/x\te1" {
		t.Fatalf("ch[0] must be the English branch: %+v", chs[0])
	}
	if chs[1].Label != "2" || chs[1].URL != "https://m/x\te2" {
		t.Fatalf("ch[1] must be the English branch: %+v", chs[1])
	}
}

func TestScraperImages(t *testing.T) {
	t.Parallel()
	s := scraperServer(t)
	imgs, err := s.GetImages(context.Background(), "https://m/x2\tc1en")
	if err != nil || len(imgs) != 2 || imgs[0][:4] != "http" {
		t.Fatalf("images = %v, %v", imgs, err)
	}
	if _, err := s.GetImages(context.Background(), "https://m/plain-url"); err == nil {
		t.Fatal("non-composite chapter url must error")
	}
}

func TestScraperSearch(t *testing.T) {
	t.Parallel()
	s := scraperServer(t)
	urls, err := s.SearchManga(context.Background(), "", "a")
	if err != nil || len(urls) != 1 || urls[0] != "https://m/a" {
		t.Fatalf("search = %v, %v", urls, err)
	}
}
