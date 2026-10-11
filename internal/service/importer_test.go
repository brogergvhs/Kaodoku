package service

import (
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
)

func TestParseChapterFile(t *testing.T) {
	cases := []struct {
		name      string
		wantLabel string
		wantNum   int
	}{
		{"Chapter 001.cbz", "1", 1},
		{"c012.cbz", "12", 12},
		{"Ch 12.5.cbz", "12.5", 12},
		{"Frieren - Chapter 023 - x.cbz", "23", 23},
		{"012.05.cbz", "12.05", 12},
		{"Oneshot.cbz", "Oneshot", 0},
		{"vol 2 ch 7.cbz", "7", 7},
		// leading chapter number must win over a trailing "part M"
		{"1: The Boy And The Girl; Part 1.cbz", "1", 1},
		{"11: The Girl; Part 1.cbz", "11", 11},
		{"10: The Girl And The Boy; Part 2.cbz", "10", 10},
		{"21.cbz", "21", 21},
		{"5 - The Strange Man.cbz", "5", 5},
		// space-separated leading number (Jujutsu Kaisen style)
		{"33 kyoto sister school goodwill event team battle part 0.cbz", "33", 33},
		{"149 perfect preparation part 2.cbz", "149", 149},
		{"165 tokyo no 1 colony part 5.cbz", "165", 165},
		{"10 after the rain.cbz", "10", 10},
		// no leading number: a trailing chapter number still works
		{"Berserk 100.cbz", "100", 100},
		// leading number wins for digit-leading names (rip convention)
		{"100 Bullets 5.cbz", "100", 100},
	}
	for _, tc := range cases {
		label, num := parseChapterFile(tc.name)
		if label != tc.wantLabel || num != tc.wantNum {
			t.Errorf("parseChapterFile(%q) = %q/%d, want %q/%d", tc.name, label, num, tc.wantLabel, tc.wantNum)
		}
	}
}

func TestDefaultMonitored(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"FINISHED", false},
		{"completed", false},
		{"Complete", false},
		{"RELEASING", true},
		{"", true},
	}
	for _, tc := range cases {
		got := defaultMonitored(catalog.Manga{Status: tc.status})
		if got != tc.want {
			t.Errorf("defaultMonitored(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestMangaStripLikely(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		m    catalog.Manga
		want bool
	}{
		{"full color tag", catalog.Manga{Tags: []string{"Male Protagonist", "Full Color"}}, true},
		{"korean origin", catalog.Manga{RawJSON: `{"countryOfOrigin":"KR"}`}, true},
		{"chinese origin", catalog.Manga{RawJSON: `{"countryOfOrigin":"CN"}`}, true},
		{"japanese manga", catalog.Manga{RawJSON: `{"countryOfOrigin":"JP"}`, Tags: []string{"Shounen"}}, false},
		{"no data", catalog.Manga{}, false},
	}
	for _, tc := range cases {
		if got := mangaStripLikely(tc.m); got != tc.want {
			t.Errorf("%s: mangaStripLikely = %v, want %v", tc.name, got, tc.want)
		}
	}
}
