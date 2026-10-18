package server

import (
	"bytes"
	"html/template"
	"testing"

	"github.com/PuerkitoBio/goquery"
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
