package mangadex

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/ui"
)

const mangaID = "30196491-8fc2-4961-8886-a58f898b1b3e"

func TestGetChaptersDedupesAndSkipsExternal(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/aggregate") {
			return response(`{"volumes":{"1":{"chapters":{"1":{"id":"agg-1"},"7":{"id":"cccccccc-0000-0000-0000-000000000007"}}}}}`), nil
		}
		if !strings.Contains(req.URL.Path, "/manga/"+mangaID+"/feed") {
			t.Fatalf("unexpected %s", req.URL)
		}
		return response(`{"total":5,"data":[
			{"id":"aaaaaaaa-0000-0000-0000-000000000001","attributes":{"chapter":"1","volume":"3","title":"Start","pages":20}},
			{"id":"aaaaaaaa-0000-0000-0000-000000000002","attributes":{"chapter":"1","title":"Dup group","pages":18}},
			{"id":"aaaaaaaa-0000-0000-0000-000000000003","attributes":{"chapter":"1.5","title":"","pages":8}},
			{"id":"aaaaaaaa-0000-0000-0000-000000000004","attributes":{"chapter":"2","title":"Elsewhere","pages":0,"externalUrl":"https://x"}},
			{"id":"aaaaaaaa-0000-0000-0000-000000000005","attributes":{"chapter":"","title":"Special","pages":9}}
		]}`), nil
	})}
	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	chapters, err := scraper.GetChapters(t.Context(), "https://mangadex.org/title/"+mangaID+"/berserk-of-gluttony")
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 3 {
		t.Fatalf("chapters = %#v", chapters)
	}
	if chapters[0].Label != "Oneshot" || chapters[1].Label != "1" || chapters[2].Label != "1.5" {
		t.Fatalf("labels = %v %v %v", chapters[0].Label, chapters[1].Label, chapters[2].Label)
	}
	// Chapter 7 exists only in another language (aggregate) — reported as gap.
	_, gap, err := scraper.GetChaptersByLanguage(t.Context(), "https://mangadex.org/title/"+mangaID+"/x", []string{"en"}, false)
	if err != nil || gap != 1 {
		t.Fatalf("gap = %d, err = %v", gap, err)
	}
	all, _, err := scraper.GetChaptersByLanguage(t.Context(), "https://mangadex.org/title/"+mangaID+"/x", []string{"en"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[3].Label != "7" || !strings.HasSuffix(all[3].URL, "cccccccc-0000-0000-0000-000000000007") {
		t.Fatalf("includeAll = %#v", all)
	}
	if chapters[1].Title != "Chapter 1 - Start" {
		t.Fatalf("dedupe kept the wrong entry: %q", chapters[1].Title)
	}
	if chapters[1].URL != "https://mangadex.org/chapter/aaaaaaaa-0000-0000-0000-000000000001" {
		t.Fatalf("URL = %q", chapters[1].URL)
	}
	if chapters[1].Volume != 3 || chapters[1].UID != "aaaaaaaa-0000-0000-0000-000000000001" {
		t.Fatalf("volume/uid = %v %q", chapters[1].Volume, chapters[1].UID)
	}
	if all[3].Volume != 1 || all[3].UID != "cccccccc-0000-0000-0000-000000000007" {
		t.Fatalf("aggregate volume/uid = %v %q", all[3].Volume, all[3].UID)
	}
}

func TestGetImagesBuildsAtHomeURLs(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/at-home/server/aaaaaaaa-0000-0000-0000-000000000001" {
			t.Fatalf("unexpected %s", req.URL)
		}
		return response(`{"baseUrl":"https://node.mangadex.network","chapter":{"hash":"h4sh","data":["1-a.jpg","2-b.png"]}}`), nil
	})}
	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	urls, err := scraper.GetImages(t.Context(), "https://mangadex.org/chapter/aaaaaaaa-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 || urls[0] != "https://node.mangadex.network/data/h4sh/1-a.jpg" {
		t.Fatalf("urls = %v", urls)
	}
}

func TestSearchMangaReturnsTitleURLs(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.mangadex.org" || req.URL.Query().Get("title") != "berserk" {
			t.Fatalf("unexpected %s", req.URL)
		}
		return response(`{"data":[{"id":"` + mangaID + `"}]}`), nil
	})}
	scraper := NewScraper(client, ui.NewLogger(false), nil, false, nil)
	urls, err := scraper.SearchManga(t.Context(), "https://api.mangadex.org/manga?limit=10&title={query}", "berserk")
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "https://mangadex.org/title/"+mangaID {
		t.Fatalf("urls = %v", urls)
	}
}

func response(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     http.Header{},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
