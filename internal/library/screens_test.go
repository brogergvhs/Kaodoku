package library

import "testing"

func TestHasAnyTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		have []string
		want []string
		out  bool
	}{
		{"empty want", []string{"Action"}, nil, false},
		{"case-insensitive", []string{"Action", "Drama"}, []string{"action"}, true},
		{"trimmed", []string{" Action "}, []string{"action "}, true},
		{"punctuation", []string{"Sci Fi"}, []string{"sci-fi"}, true},
		{"no match", []string{"Horror"}, []string{"Drama"}, false},
		{"no partial match", []string{"Drama"}, []string{"Dramatic"}, false},
		{"empty have", nil, []string{"x"}, false},
	}
	for _, tc := range cases {
		if got := HasAnyTag(tc.have, tc.want); got != tc.out {
			t.Errorf("%s: HasAnyTag(%v, %v) = %v, want %v", tc.name, tc.have, tc.want, got, tc.out)
		}
	}
}

func TestScreenConfigMatches(t *testing.T) {
	t.Parallel()

	adult := Title{IsAdult: true, ContentTags: []string{"Gore"}}
	drama := Title{ContentTags: []string{"Drama"}}

	if !(ScreenConfig{}).Matches(adult) || !(ScreenConfig{}).Matches(drama) {
		t.Error("empty config should match any title")
	}
	if (ScreenConfig{Adult: "only"}).Matches(drama) || !(ScreenConfig{Adult: "only"}).Matches(adult) {
		t.Error("adult=only should keep only adult titles")
	}
	if (ScreenConfig{Adult: "exclude"}).Matches(adult) || !(ScreenConfig{Adult: "exclude"}).Matches(drama) {
		t.Error("adult=exclude should drop adult titles")
	}
	if (ScreenConfig{IncludeTags: []string{"drama"}}).Matches(adult) || !(ScreenConfig{IncludeTags: []string{"drama"}}).Matches(drama) {
		t.Error("include tags should require a case-insensitive match")
	}
	if (ScreenConfig{IncludeTags: []string{"gore"}, ExcludeTags: []string{"gore"}}).Matches(adult) {
		t.Error("exclude should win over a matching include")
	}
}
