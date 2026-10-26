package server

import (
	"bytes"
	"html/template"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// tableColumn is a header cell. A non-empty SortKey makes it sortable.
type tableColumn struct {
	Label   string
	Class   string
	SortKey string
}

// tableRow is one data row plus optional collapsible detail content.
type tableRow struct {
	ID     string
	Class  string // extra classes for the <tr> (e.g. state tint)
	Cells  []template.HTML
	Detail template.HTML
}

// tableData drives the reusable paginated/sortable/collapsible "table" template.
type tableData struct {
	ID      string // dom id
	BaseURL string // hx-get base for pagination and sort
	Params  url.Values
	Columns []tableColumn
	Rows    []tableRow
	Page    int
	PerPage int
	Total   int
	Sort    string
	Dir     string
	Empty   string // message when there are no rows
	Poll    bool   // auto-refresh the current page (e.g. while jobs run)
}

func (t tableData) TotalPages() int {
	if t.PerPage <= 0 {
		return 1
	}
	pages := (t.Total + t.PerPage - 1) / t.PerPage
	if pages < 1 {
		return 1
	}
	return pages
}
func (t tableData) HasPrev() bool { return t.Page > 1 }
func (t tableData) HasNext() bool { return t.Page < t.TotalPages() }
func (t tableData) Prev() int     { return t.Page - 1 }
func (t tableData) Next() int     { return t.Page + 1 }
func (t tableData) HasDetails() bool {
	for _, r := range t.Rows {
		if r.Detail != "" {
			return true
		}
	}
	return false
}
func (t tableData) ColSpan() int {
	n := len(t.Columns)
	if t.HasDetails() {
		n++
	}
	return n
}
func (t tableData) RangeStart() int {
	if t.Total == 0 {
		return 0
	}
	return (t.Page-1)*t.PerPage + 1
}
func (t tableData) RangeEnd() int {
	end := t.Page * t.PerPage
	if end > t.Total {
		end = t.Total
	}
	return end
}

func (t tableData) PageURL(page int) template.URL {
	return t.url(page, t.Sort, t.Dir)
}

// SortURL toggles the sort direction for a column and resets to page 1.
func (t tableData) SortURL(key string) template.URL {
	dir := "asc"
	if t.Sort == key && t.Dir == "asc" {
		dir = "desc"
	}
	return t.url(1, key, dir)
}

func (t tableData) SortMark(key string) string {
	if t.Sort != key {
		return ""
	}
	if t.Dir == "desc" {
		return " ▼"
	}
	return " ▲"
}

func (t tableData) url(page int, sort, dir string) template.URL {
	q := url.Values{}
	for key, values := range t.Params {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	q.Set("page", strconv.Itoa(page))
	if sort != "" {
		q.Set("sort", sort)
		q.Set("dir", dir)
	}
	return template.URL(t.BaseURL + "?" + q.Encode())
}

// renderToHTML executes a named template to trusted HTML for embedding in a
// table cell or detail panel.
func (u *webUI) renderToHTML(name string, data any) template.HTML {
	var buf bytes.Buffer
	if err := u.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return template.HTML(template.HTMLEscapeString(err.Error()))
	}
	return template.HTML(buf.String())
}

// tableParams reads paging/sort query params with sane defaults.
func tableParams(values url.Values, perPage int) (page int, sort, dir string) {
	page, _ = strconv.Atoi(values.Get("page"))
	if page < 1 {
		page = 1
	}
	sort = values.Get("sort")
	dir = values.Get("dir")
	if dir != "desc" {
		dir = "asc"
	}
	return page, sort, dir
}

// paginate returns the slice for the requested page, clamping the page.
func paginate[T any](items []T, page, perPage int) ([]T, int) {
	total := len(items)
	start := (page - 1) * perPage
	if start >= total {
		start = max(0, ((total-1)/perPage)*perPage)
	}
	if start < 0 {
		start = 0
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return items[start:end], total
}

func sortChapters(cs []library.ChapterStatus, key, dir string) {
	var less func(a, b library.ChapterStatus) bool
	switch key {
	case "number":
		less = func(a, b library.ChapterStatus) bool {
			if a.NumberMain != b.NumberMain {
				return a.NumberMain < b.NumberMain
			}
			return a.SuffixNum < b.SuffixNum
		}
	case "label":
		less = func(a, b library.ChapterStatus) bool { return util.NaturalLess(a.Label, b.Label) }
	case "read":
		less = func(a, b library.ChapterStatus) bool { return chapterReadPercent(a) < chapterReadPercent(b) }
	case "missing":
		less = func(a, b library.ChapterStatus) bool {
			if a.Downloaded != b.Downloaded {
				return !a.Downloaded // missing chapters first
			}
			if a.NumberMain != b.NumberMain {
				return a.NumberMain < b.NumberMain
			}
			return a.SuffixNum < b.SuffixNum
		}
	case "downloaded":
		less = func(a, b library.ChapterStatus) bool {
			at, bt := time.Time{}, time.Time{}
			if a.DownloadedAt != nil {
				at = *a.DownloadedAt
			}
			if b.DownloadedAt != nil {
				bt = *b.DownloadedAt
			}
			return at.Before(bt)
		}
	default:
		return
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if dir == "desc" {
			return less(cs[j], cs[i])
		}
		return less(cs[i], cs[j])
	})
}
