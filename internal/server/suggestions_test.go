package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
)

// The tracker-suggestions collapsible renders nothing when there is nothing
// to suggest, and honors the page's current view otherwise.
func TestAnilistSuggestionsTemplate(t *testing.T) {
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))
	render := func(v mangaResults) string {
		var buf bytes.Buffer
		if err := u.tmpl.ExecuteTemplate(&buf, "trackerSuggestions", v); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	if out := strings.TrimSpace(render(mangaResults{})); out != "" {
		t.Fatalf("empty suggestions rendered %q", out)
	}

	items := []searchResultView{{Manga: catalog.Manga{ProviderID: "9", TitleRomaji: "Berserk", CoverImage: "x.jpg", Format: "MANGA"}}}
	markers := map[string]string{
		"cards": "grid-cols-2",
		"table": "<table",
		"full":  "h-52 w-36",
	}
	for view, marker := range markers {
		out := render(mangaResults{View: view, Items: items, CanAdd: true})
		for _, want := range []string{"collapse-title", "1 not in library", "Berserk", marker, ">Add<"} {
			if !strings.Contains(out, want) {
				t.Fatalf("view %s: missing %q in:\n%s", view, want, out)
			}
		}
	}
}
