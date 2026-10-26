package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
)

func TestRelatedMangaLabels(t *testing.T) {
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))
	cases := []struct{ relation, label string }{
		{"PREQUEL", "Prequel"}, {"SEQUEL", "Sequel"}, {"SIDE_STORY", "Side story"},
		{"SPIN_OFF", "Spin-off"}, {"ALTERNATIVE", "Alternative version"}, {"OTHER", "Other"},
	}
	for _, tc := range cases {
		for _, show := range []bool{true, false} {
			view := mangaResults{View: "cards", ShowRelations: show, Items: []searchResultView{{Manga: catalog.Manga{ProviderID: "1", TitleRomaji: "Test manga", RelationType: tc.relation}}}}
			var buf bytes.Buffer
			if err := u.tmpl.ExecuteTemplate(&buf, "mangaResults", view); err != nil {
				t.Fatal(err)
			}
			if found := strings.Contains(buf.String(), ">"+tc.label+"</span>"); found != show {
				t.Errorf("relation %q, ShowRelations=%v: label %q visible=%v", tc.relation, show, tc.label, found)
			}
		}
	}
}
