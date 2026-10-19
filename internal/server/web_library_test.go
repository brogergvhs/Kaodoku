package server

import (
	"net/url"
	"reflect"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/library"
)

func TestLibraryControlsFromTags(t *testing.T) {
	t.Parallel()

	values := url.Values{
		"include_tags": {"Action", "Sci-Fi"},
		"exclude_tags": {"Gore, Horror"}, // comma fallback input
	}
	c := libraryControlsFrom(values)
	if want := []string{"Action", "Sci-Fi"}; !reflect.DeepEqual(c.IncludeTags, want) {
		t.Errorf("IncludeTags = %v, want %v", c.IncludeTags, want)
	}
	if want := []string{"Gore", "Horror"}; !reflect.DeepEqual(c.ExcludeTags, want) {
		t.Errorf("ExcludeTags = %v, want %v", c.ExcludeTags, want)
	}
}

func TestTagList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{"Action", "Sci-Fi"}, []string{"Action", "Sci-Fi"}},
		{[]string{"Gore, Horror"}, []string{"Gore", "Horror"}},
		{[]string{"A,B", "C"}, []string{"A", "B", "C"}},
		{[]string{" ", "", " ,A , "}, []string{"A"}},
	}
	for _, tc := range cases {
		if got := tagList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tagList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestScreenDefaults(t *testing.T) {
	t.Parallel()

	screen := &library.Screen{Config: library.ScreenConfig{
		IncludeTags: []string{"Action"},
		ExcludeTags: []string{"Gore"},
		Sort:        "rating",
		Monitor:     "on",
	}}

	c := libraryControlsFrom(screenDefaults(url.Values{}, screen))
	if c.IncludeTags != nil || c.ExcludeTags != nil {
		t.Errorf("screen tags leaked into ad-hoc controls: include=%v exclude=%v", c.IncludeTags, c.ExcludeTags)
	}
	if c.Sort != "rating" || c.Monitor != "on" {
		t.Errorf("scalar defaults not applied: sort=%q monitor=%q", c.Sort, c.Monitor)
	}

	c = libraryControlsFrom(screenDefaults(url.Values{"sort": {"title"}}, screen))
	if c.Sort != "title" {
		t.Errorf("explicit URL value should win over screen default, got sort=%q", c.Sort)
	}

	if c := libraryControlsFrom(screenDefaults(url.Values{}, nil)); c.Sort != "added" {
		t.Errorf("nil screen should fall through to plain defaults, got sort=%q", c.Sort)
	}
}

func TestTableDataPageURLCarriesTagFilters(t *testing.T) {
	t.Parallel()

	td := tableData{
		BaseURL: "/ui/library/table",
		Params: url.Values{
			"q":            {"one piece"},
			"include_tags": {"Action", "Sci-Fi"},
			"exclude_tags": {"Gore"},
		},
		Sort: "rating", Dir: "desc",
	}
	u, err := url.Parse(string(td.PageURL(2)))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := q["include_tags"]; !reflect.DeepEqual(got, []string{"Action", "Sci-Fi"}) {
		t.Errorf("include_tags = %v, want [Action Sci-Fi]", got)
	}
	if q.Get("exclude_tags") != "Gore" || q.Get("q") != "one piece" {
		t.Errorf("filters dropped: %v", q)
	}
	if q.Get("page") != "2" || q.Get("sort") != "rating" || q.Get("dir") != "desc" {
		t.Errorf("page/sort/dir = %s/%s/%s, want 2/rating/desc", q.Get("page"), q.Get("sort"), q.Get("dir"))
	}
}

func TestLibraryTableParamsTags(t *testing.T) {
	t.Parallel()

	values := url.Values{
		"q":            {"one piece"},
		"include_tags": {"Action", "Sci-Fi"},
		"exclude_tags": {" ", "Gore"},
		"page":         {"3"}, // not carried; pagination adds it per link
	}
	params := libraryTableParams(values)
	if got := params["include_tags"]; !reflect.DeepEqual(got, []string{"Action", "Sci-Fi"}) {
		t.Errorf("include_tags = %v, want [Action Sci-Fi]", got)
	}
	if got := params["exclude_tags"]; !reflect.DeepEqual(got, []string{"Gore"}) {
		t.Errorf("exclude_tags = %v, want [Gore]", got)
	}
	if params.Get("page") != "" {
		t.Errorf("page carried into params, want dropped")
	}
}
