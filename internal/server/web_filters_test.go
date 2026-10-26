package server

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"slices"
	"strconv"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/catalog"
)

func TestFilterFormsPreserveModalControls(t *testing.T) {
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))
	tags := []catalog.ContentTag{{Name: "Action", Kind: "genre"}}
	for _, page := range []string{"library", "search"} {
		views := []string{"table", "cards", "full"}
		if page == "library" {
			views = append(views, "auto")
		}
		for _, view := range views {
			t.Run(page+"/"+view, func(t *testing.T) {
				var data any = searchView{Controls: searchControls{View: view, Dir: "desc", IncludeTags: []string{"Action"}}, TagOptions: tags}
				if page == "library" {
					data = libraryView{Controls: libraryControls{View: view, Dir: "desc", IncludeTags: []string{"Action"}}, TagOptions: tags}
				}
				var buf bytes.Buffer
				if err := u.tmpl.ExecuteTemplate(&buf, page, data); err != nil {
					t.Fatal(err)
				}
				doc, err := goquery.NewDocumentFromReader(&buf)
				if err != nil {
					t.Fatal(err)
				}
				form := doc.Find("form.filter-form")
				if form.Length() != 1 || form.Find("dialog.filter-modal").Length() != 1 {
					t.Fatal("filter dialog must remain inside the filter form")
				}
				if form.Find("form").Length() != 0 {
					t.Fatal("nested forms break filter submission")
				}
				checked := form.Find(`input[name="view"]:checked`)
				if value, _ := checked.Attr("value"); checked.Length() != 1 || value != view {
					t.Fatalf("selected view = %q, want %q", value, view)
				}
				if form.Find(`input[name="view"]`).Length() != len(views) {
					t.Fatal("view choices must not be duplicated for mobile")
				}
				if form.Find(`select[name="sort"]`).Length() != 1 || form.Find(`input[name="dir"]:checked`).Length() != 1 {
					t.Fatal("sort controls must remain associated with the form")
				}
				if form.Find(`dialog input[name="include_tags"][value="Action"]`).Length() != 1 {
					t.Fatal("modal must retain selected tags")
				}
				form.Find("dialog button").Each(func(_ int, button *goquery.Selection) {
					if typ, _ := button.Attr("type"); typ != "button" {
						t.Error("dialog actions must not submit the filter form")
					}
				})
			})
		}
	}
}

func TestExcludeBlockedAddsGuardTagsToProviderFilter(t *testing.T) {
	ctx := auth.WithUser(context.Background(), &auth.User{BlockedTags: []string{"Adult", "Smut", "Gore"}})
	options := []catalog.ContentTag{{Name: "Adult", Kind: "genre"}, {Name: "Smut", Kind: "genre"}, {Name: "Gore", Kind: "tag"}}
	filter := catalog.SearchFilter{GenreNotIn: []string{"Comedy"}}
	excludeBlocked(ctx, &filter, options)
	if !slices.Equal(filter.GenreNotIn, []string{"Comedy", "Adult", "Smut"}) || !slices.Equal(filter.TagNotIn, []string{"Gore"}) {
		t.Fatalf("filter = %+v", filter)
	}
}

func TestSearchHidesLibraryByDefault(t *testing.T) {
	if !searchControlsFrom(nil).HideLibrary || searchControlsFrom(map[string][]string{"hide_library": {"false"}}).HideLibrary ||
		!searchControlsFrom(map[string][]string{"hide_library": {"false", "true"}}).HideLibrary {
		t.Fatal("hide_library: default on, unchecked off, checked on")
	}
}

func TestFillPageShowsEvenPagesWithoutLosingResults(t *testing.T) {
	pages := map[int]int{1: 10, 2: 13, 3: 25, 4: 4}
	fetch := func(p int) ([]catalog.Manga, bool, error) {
		var out []catalog.Manga
		for i := range pages[p] {
			out = append(out, catalog.Manga{ProviderID: strconv.Itoa(p*100 + i)})
		}
		return out, p < 4, nil
	}
	seen := map[string]bool{}
	page, skip, total := 1, 0, 0
	for loads := 0; page != 0; loads++ {
		items, next, nextSkip, err := fillPage(page, skip, fetch)
		if err != nil || (next != 0 && len(items) != searchPerPage) || loads > 5 {
			t.Fatalf("load %d: %d items, next %d, err %v", loads, len(items), next, err)
		}
		for _, m := range items {
			if seen[m.ProviderID] {
				t.Fatalf("%s shown twice", m.ProviderID)
			}
			seen[m.ProviderID] = true
		}
		page, skip, total = next, nextSkip, total+len(items)
	}
	if total != 52 {
		t.Fatalf("showed %d of 52 results", total)
	}
	if _, next, _, err := fillPage(1, 0, func(int) ([]catalog.Manga, bool, error) { return nil, false, errors.New("down") }); err == nil || next != 0 {
		t.Fatal("first-page error must surface")
	}
}
