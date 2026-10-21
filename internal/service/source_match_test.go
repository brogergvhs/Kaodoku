package service

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/sources"
)

func TestCandidateSourceURLsReplacesSlugOnlySample(t *testing.T) {
	urls := candidateSourceURLs(sources.Source{Profile: sources.Profile{
		ID:             "demo",
		SampleMangaURL: "https://demo.test/manga/sample-title/",
	}}, catalog.Manga{
		TitleEnglish: "The Great Mage Returns After 4000 Years",
		Synonyms:     []string{"Archmage Returns"},
	})
	if len(urls) == 0 || urls[0] != "https://demo.test/manga/the-great-mage-returns-after-4000-years" {
		t.Fatalf("urls = %#v", urls)
	}
	if strings.Contains(strings.Join(urls, ","), "sample-title") {
		t.Fatalf("sample slug leaked into urls: %#v", urls)
	}
}

func TestCandidateSourceURLsSkipsNumericOrOpaquePatterns(t *testing.T) {
	for _, sample := range []string{
		"https://mangapill.com/manga/5281/sakamoto-days",
		"https://weebcentral.com/series/01J76XYDMTRNJZJH9G1ADMPJQC/The-Great-Mage-Returns-After-4000-Years",
	} {
		urls := candidateSourceURLs(sources.Source{Profile: sources.Profile{SampleMangaURL: sample}}, catalog.Manga{TitleEnglish: "Demo"})
		if len(urls) != 0 {
			t.Fatalf("candidateSourceURLs(%q) = %#v", sample, urls)
		}
	}
}

func TestSearchLinksKeepsLikelyMangaResults(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`
		<html><body>
			<a href="/manga/the-archmage-returns-after-4000-years/">The Archmage Returns After 4000 Years</a>
			<a href="/manga/the-archmage-returns-after-4000-years/c263">The Archmage Returns After 4000 Years Chapter 263</a>
			<a href="/manga/other-title/">Other Title</a>
			<a href="/privacy">The Archmage Returns After 4000 Years</a>
		</body></html>
	`))
	if err != nil {
		t.Fatal(err)
	}

	urls := searchLinks(doc, sources.Source{Profile: sources.Profile{
		BaseURL:        "https://coffeemanga.net/",
		SampleMangaURL: "https://coffeemanga.net/manga/im-being-raised-by-villains/",
	}}, catalog.Manga{
		TitleEnglish: "The Archmage Returns After 4000 Years",
	})
	if len(urls) != 1 || urls[0] != "https://coffeemanga.net/manga/the-archmage-returns-after-4000-years/" {
		t.Fatalf("urls = %#v", urls)
	}
}

func TestSearchStructuredLinksUsesSlugResults(t *testing.T) {
	urls := searchStructuredLinks(`{"data":[
		{"title":"The Demonic Supreme Sword Duplicate","slug":"02-the-demonic-supreme-sword","chapter_count":48},
		{"title":"The Demonic Supreme Sword","slug":"the-demonic-supreme-sword","chapter_count":270}
	]}`, sources.Source{Profile: sources.Profile{
		BaseURL:        "https://comickz.co.uk/",
		SampleMangaURL: "https://comickz.co.uk/comic/the-demonic-supreme-sword",
	}}, catalog.Manga{TitleEnglish: "The Demonic Supreme Sword"})
	if len(urls) != 2 || urls[0] != "https://comickz.co.uk/comic/the-demonic-supreme-sword" {
		t.Fatalf("urls = %#v", urls)
	}
}

func TestSearchStructuredLinksUsesPublicURLResults(t *testing.T) {
	urls := searchStructuredLinks(`{"items":[{"title":"Academy's Genius Swordmaster","public_url":"/comics/academys-genius-swordmaster"}]}`, sources.Source{Profile: sources.Profile{
		BaseURL:        "https://asurascans.com/",
		SampleMangaURL: "https://asurascans.com/comics/academys-genius-swordmaster",
	}}, catalog.Manga{TitleEnglish: "Academy's Genius Swordmaster"})
	if len(urls) != 1 || urls[0] != "https://asurascans.com/comics/academys-genius-swordmaster" {
		t.Fatalf("urls = %#v", urls)
	}
}

func TestMatchesWantedTitleAllowsPartialTitleOverlap(t *testing.T) {
	manga := catalog.Manga{TitleEnglish: "The Archmage Returns After 4000 Years"}
	if !matchesWantedTitle(manga, "https://demo.test/manga/the-great-mage-returns-after-4000-years") {
		t.Fatalf("wanted title did not match alternate slug")
	}
	if matchesWantedTitle(manga, "https://demo.test/manga/solo-leveling") {
		t.Fatalf("unrelated title matched")
	}
}

// A short trailing token (the "D" in "Initial D") must not raise the match
// threshold beyond what the countable words can ever satisfy.
func TestMatchesWantedTitleShortTitle(t *testing.T) {
	manga := catalog.Manga{TitleEnglish: "Initial D"}
	if !matchesWantedTitle(manga, "https://mangakatana.com/manga/initial-d.20114") {
		t.Fatalf("short title did not match on HTML search source")
	}
	if matchesWantedTitle(manga, "https://mangakatana.com/manga/one-piece.100") {
		t.Fatalf("unrelated title matched short title")
	}
}

// Sample URLs whose identifying segment is an opaque id (UUIDs included) must
// not produce slug-probe guesses: the site ignores the slug, so a swapped
// slug resolves to the SAME manga under a wrong name.
func TestCandidateSourceURLsRejectsOpaqueIDs(t *testing.T) {
	manga := catalog.Manga{TitleRomaji: "Dr. STONE"}
	cases := map[string]int{
		"https://mangadex.org/title/30196491-8fc2-4961-8886-a58f898b1b3e/berserk-of-gluttony": 0,
		"https://site.test/comics/12345/berserk":                                              0,
		"https://site.test/manga/berserk-of-gluttony":                                         1,
	}
	for sample, want := range cases {
		src := sources.Source{Profile: sources.Profile{SampleMangaURL: sample}}
		if got := len(candidateSourceURLs(src, manga)); got != want {
			t.Errorf("candidateSourceURLs(%s) = %d guesses, want %d", sample, got, want)
		}
	}
}
