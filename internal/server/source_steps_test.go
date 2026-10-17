package server

import (
	"html/template"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
)

// A failed title update marks the source broken without touching the stored
// verify steps, so the reason must show next to the passing pipeline.
func TestSourceStepsAlwaysShowLastError(t *testing.T) {
	t.Parallel()
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	passing := []sources.VerifyStep{
		{Name: "search", Status: sources.StepOK},
		{Name: "pick manga", Status: sources.StepOK},
		{Name: "chapters", Status: sources.StepOK},
		{Name: "images", Status: sources.StepOK},
	}
	src := sources.Source{
		Profile:     sources.Profile{Enabled: true},
		Status:      sources.StatusBroken,
		VerifySteps: passing,
		LastError:   `while updating "Some Title": fetch chapters: HTTP 404`,
	}
	html := string(u.renderToHTML("sourceSteps", src))
	if !strings.Contains(html, "Some Title") || !strings.Contains(html, "HTTP 404") {
		t.Fatalf("broken source hides its reason: %s", html)
	}
	if !strings.Contains(html, "re-run Verify") {
		t.Fatalf("error after a passing verify should say so: %s", html)
	}

	clean := sources.Source{Status: sources.StatusHealthy, VerifySteps: passing}
	if h := string(u.renderToHTML("sourceSteps", clean)); strings.Contains(h, "last error") {
		t.Fatalf("healthy source should show no error block: %s", h)
	}

	// The detail row is collapsed by default, so the badge carries the reason too.
	row := string(u.renderToHTML("sourceRow", sourceRowView{Source: src}))
	if !strings.Contains(row, `title="while updating &#34;Some Title&#34;: fetch chapters: HTTP 404"`) {
		t.Fatalf("status badge missing the reason tooltip: %s", row)
	}
}

// A failed verify already explains itself through the step log, so the error
// must not be repeated with a hint claiming it happened afterwards.
func TestSourceStepsDoesNotRepeatVerifyFailure(t *testing.T) {
	t.Parallel()
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	src := sources.Source{
		Profile: sources.Profile{Enabled: true},
		Status:  sources.StatusBroken,
		VerifySteps: []sources.VerifyStep{
			{Name: "search", Status: sources.StepOK},
			{Name: "images", Status: sources.StepFailed, Log: "GET img: HTTP 403"},
		},
		LastError: "sample image download failed: GET img: HTTP 403",
	}
	html := string(u.renderToHTML("sourceSteps", src))
	if strings.Count(html, "HTTP 403") != 1 {
		t.Fatalf("verify failure should appear once: %s", html)
	}
	if strings.Contains(html, "after the verification above") {
		t.Fatalf("hint is wrong for a verify failure: %s", html)
	}
}

// A later title success flips the badge green; the stale red step needs saying.
func TestSourceStepsExplainsHealthyBadgeOverFailedStep(t *testing.T) {
	t.Parallel()
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	src := sources.Source{
		Profile: sources.Profile{Enabled: true},
		Status:  sources.StatusHealthy,
		VerifySteps: []sources.VerifyStep{
			{Name: "chapters", Status: sources.StepOK},
			{Name: "images", Status: sources.StepFailed, Log: "GET img: HTTP 403"},
		},
	}
	if html := string(u.renderToHTML("sourceSteps", src)); !strings.Contains(html, "badge is green") {
		t.Fatalf("green badge over a failed step is unexplained: %s", html)
	}
}

// One poller for the whole kotatsu table, armed only while a row is verifying:
// a bulk import would otherwise start one poll loop per imported row.
func TestKotatsuTablePollsOnceWhileVerifying(t *testing.T) {
	t.Parallel()
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	rows := []sourceRowView{
		{Source: sources.Source{Profile: sources.Profile{ID: "kotatsu:a", Enabled: true}}},
		{Source: sources.Source{Profile: sources.Profile{ID: "kotatsu:b", Enabled: true}}, Active: true},
		{Source: sources.Source{Profile: sources.Profile{ID: "kotatsu:c", Enabled: true}}},
	}
	html := string(u.renderToHTML("kotatsuSourcesTable", rows))
	if n := strings.Count(html, "/ui/sources/kotatsu/table"); n != 1 {
		t.Fatalf("got %d table pollers, want exactly 1: %s", n, html)
	}
	if strings.Contains(html, "/row") {
		t.Fatalf("rows must not poll individually: %s", html)
	}

	rows[1].Active = false
	idle := string(u.renderToHTML("kotatsuSourcesTable", rows))
	if strings.Contains(idle, "/ui/sources/kotatsu/table") {
		t.Fatalf("idle table should not poll: %s", idle)
	}
}

// The main table resolves page-load "verifying" rows the same way the
// kotatsu table does: one table-level poller, no per-row polling.
func TestSourcesTablePollsOnceWhileVerifying(t *testing.T) {
	t.Parallel()
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	rows := []sourceRowView{
		{Source: sources.Source{Profile: sources.Profile{ID: "a", Enabled: true}}},
		{Source: sources.Source{Profile: sources.Profile{ID: "b", Enabled: true}}, Active: true},
	}
	html := string(u.renderToHTML("sourcesTable", rows))
	if n := strings.Count(html, "/ui/sources/table"); n != 1 {
		t.Fatalf("got %d table pollers, want exactly 1: %s", n, html)
	}
	if strings.Contains(html, "/row") {
		t.Fatalf("rows must not poll individually: %s", html)
	}

	rows[1].Active = false
	if idle := string(u.renderToHTML("sourcesTable", rows)); strings.Contains(idle, "/ui/sources/table") {
		t.Fatalf("idle table should not poll: %s", idle)
	}
}

func TestSourceRowShowsTitleCount(t *testing.T) {
	t.Parallel()
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	src := sources.Source{Profile: sources.Profile{ID: "x", Enabled: true}}
	if h := string(u.renderToHTML("sourceRow", sourceRowView{Source: src, Titles: 3})); !strings.Contains(h, "3") || !strings.Contains(h, "titles") {
		t.Fatalf("row should show the title count: %s", h)
	}
	if h := string(u.renderToHTML("sourceRow", sourceRowView{Source: src, Titles: 1})); !strings.Contains(h, ">title<") {
		t.Fatalf("one title should be singular: %s", h)
	}
	if h := string(u.renderToHTML("sourceRow", sourceRowView{Source: src})); strings.Contains(h, "titles") {
		t.Fatalf("unused source should show a dash, not a count: %s", h)
	}
}

func TestSourceRowShowsFetchIssues(t *testing.T) {
	t.Parallel()
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))

	src := sources.Source{Profile: sources.Profile{ID: "x", Enabled: true}}
	h := string(u.renderToHTML("sourceRow", sourceRowView{Source: src, FetchFails: 2}))
	if !strings.Contains(h, "2 title issues") || !strings.Contains(h, "/ui/sources/x/title-errors") {
		t.Fatalf("row should show the issue badge and lazy-load the details: %s", h)
	}
	if h := string(u.renderToHTML("sourceRow", sourceRowView{Source: src})); strings.Contains(h, "title issues") || strings.Contains(h, "title-errors") {
		t.Fatalf("clean source should show no issue badge: %s", h)
	}

	list := string(u.renderToHTML("sourceTitleErrors", []library.Title{{ID: 7, DisplayTitle: "T", FetchError: `<script>alert(1)</script>`}}))
	if strings.Contains(list, "<script>") || !strings.Contains(list, "&lt;script&gt;") {
		t.Fatalf("fetch error must be escaped: %s", list)
	}
	if !strings.Contains(list, "/library/7") {
		t.Fatalf("failing title should link to its library page: %s", list)
	}
}
