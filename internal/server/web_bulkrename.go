package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/library"
)

// bulkLineRe parses one pasted line: <number>: "<name>", vol: <number>
// — the volume part is optional, the name may be empty.
var bulkLineRe = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*:\s*"(.*)"\s*(?:,\s*(?i:vol)\s*:\s*([0-9]+(?:\.[0-9]+)?)\s*)?$`)

// bulkRenameRow is one preview row: a chapter pick (0 = unmatched, user must
// select), the pasted name, the optional volume, and the line's own parse
// problem, shown inline under the row.
type bulkRenameRow struct {
	ChapterID int64
	Number    string
	Name      string
	Volume    string
	Error     string
}

type bulkRenameView struct {
	TitleID  int64
	Errors   []string
	Rows     []bulkRenameRow
	Chapters []library.ChapterStatus
}

// parseBulkRename turns pasted text into preview rows, matching chapter
// numbers against the title's labels (exact label first, then numeric
// equality so "040" still finds "40").
func parseBulkRename(text string, chapters []library.ChapterStatus) []bulkRenameRow {
	byLabel := make(map[string]int64, len(chapters))
	byNumber := make(map[float64]int64, len(chapters))
	for _, ch := range chapters {
		byLabel[ch.Label] = ch.ID
		if n, err := strconv.ParseFloat(strings.TrimSpace(ch.Label), 64); err == nil {
			if _, dup := byNumber[n]; !dup {
				byNumber[n] = ch.ID
			}
		}
	}
	var rows []bulkRenameRow
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := bulkLineRe.FindStringSubmatch(line)
		if m == nil {
			// Keep the raw line in the name field so nothing pasted is lost.
			rows = append(rows, bulkRenameRow{Name: line,
				Error: `Couldn't parse this line — expected <number>: "<name>", vol: <number>`})
			continue
		}
		row := bulkRenameRow{Number: m[1], Name: m[2], Volume: m[3]}
		if id, ok := byLabel[m[1]]; ok {
			row.ChapterID = id
		} else if n, err := strconv.ParseFloat(m[1], 64); err == nil {
			if id, ok := byNumber[n]; ok {
				row.ChapterID = id
			}
		}
		if row.ChapterID == 0 {
			row.Error = fmt.Sprintf("No chapter %s in this title — pick one", m[1])
		}
		rows = append(rows, row)
	}
	return rows
}

// chapterBulkRenameForm serves the paste form into the bulk-rename modal.
func (u *webUI) chapterBulkRenameForm(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil || !titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return
	}
	u.frag(w, "bulkRenamePaste", bulkRenameView{TitleID: titleID})
}

// chapterBulkRenamePreview parses the pasted text into editable rows.
func (u *webUI) chapterBulkRenamePreview(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil || !titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return
	}
	chs, err := u.svc.TitleChapters(r.Context(), titleID)
	if err != nil {
		u.fail(w, err)
		return
	}
	rows := parseBulkRename(r.FormValue("text"), chs)
	var errs []string
	if len(rows) == 0 {
		errs = append(errs, "no usable lines found")
	}
	u.frag(w, "bulkRenamePreview", bulkRenameView{
		TitleID: titleID, Errors: errs, Rows: rows, Chapters: chs,
	})
}

// chapterBulkRenameApply validates the edited rows and applies them.
func (u *webUI) chapterBulkRenameApply(w http.ResponseWriter, r *http.Request) {
	titleID, err := pathID(r)
	if err != nil || !titleAllowed(r.Context(), u.svc, titleID) {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	ids, names, vols := r.Form["chapter_id"], r.Form["name"], r.Form["vol"]
	if len(ids) == 0 || len(ids) != len(names) || len(ids) != len(vols) {
		u.fail(w, fmt.Errorf("malformed bulk rename form"))
		return
	}
	seen := make(map[int64]bool, len(ids))
	items := make([]library.ChapterRename, 0, len(ids))
	for i := range ids {
		id, err := strconv.ParseInt(ids[i], 10, 64)
		if err != nil || id <= 0 {
			u.fail(w, fmt.Errorf("row %d has no chapter selected", i+1))
			return
		}
		if seen[id] {
			u.fail(w, fmt.Errorf("a chapter is selected twice"))
			return
		}
		seen[id] = true
		var vol *float64
		if v := strings.TrimSpace(vols[i]); v != "" {
			parsed, err := strconv.ParseFloat(v, 64)
			if err != nil || parsed < 0 {
				u.fail(w, fmt.Errorf("row %d has an invalid volume", i+1))
				return
			}
			vol = &parsed
		}
		items = append(items, library.ChapterRename{ChapterID: id, Title: names[i], Volume: vol})
	}
	if err := u.svc.BulkRenameChapters(r.Context(), titleID, items); err != nil {
		u.fail(w, err)
		return
	}
	u.writeChaptersTable(w, r, titleID)
}
