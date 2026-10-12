package server

import (
	"testing"

	"github.com/brogergvhs/kaodoku/internal/library"
)

func bulkCh(id int64, label string) library.ChapterStatus {
	return library.ChapterStatus{Chapter: library.Chapter{ID: id, Label: label}}
}

func TestParseBulkRename(t *testing.T) {
	t.Parallel()
	chs := []library.ChapterStatus{bulkCh(11, "40"), bulkCh(12, "54.5")}

	rows := parseBulkRename(`
40: "Battlefield Hardline 3", vol: 9
54.5: "Greater Cause 2", Vol: 12
040: ""
99: "Unknown", vol: 1
garbage line
`, chs)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5 (unparseable lines keep a row)", len(rows))
	}
	if rows[0].ChapterID != 11 || rows[0].Name != "Battlefield Hardline 3" || rows[0].Volume != "9" {
		t.Fatalf("row 0 = %+v", rows[0])
	}
	// Decimal label, case-insensitive vol keyword.
	if rows[1].ChapterID != 12 || rows[1].Volume != "12" {
		t.Fatalf("row 1 = %+v", rows[1])
	}
	// Numeric match: "040" finds label "40"; empty name and no volume valid.
	if rows[2].ChapterID != 11 || rows[2].Name != "" || rows[2].Volume != "" {
		t.Fatalf("row 2 = %+v", rows[2])
	}
	// Unknown number: row kept with no chapter picked, inline error set.
	if rows[3].ChapterID != 0 || rows[3].Name != "Unknown" || rows[3].Error == "" {
		t.Fatalf("row 3 = %+v", rows[3])
	}
	// Unparseable line: raw text preserved in the name, inline error set.
	if rows[4].ChapterID != 0 || rows[4].Name != "garbage line" || rows[4].Error == "" {
		t.Fatalf("row 4 = %+v", rows[4])
	}
	// Matched rows carry no error.
	if rows[0].Error != "" || rows[2].Error != "" {
		t.Fatalf("clean rows carry errors: %+v / %+v", rows[0], rows[2])
	}
}
