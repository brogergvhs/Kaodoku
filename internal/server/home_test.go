package server

import (
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/library"
)

func TestContinueReadingRowOrderAndLinks(t *testing.T) {
	now := time.Now()
	titles := []library.Title{
		{ID: 1, DisplayTitle: "Chapters", ReadCount: 3, DiscoveredCount: 10},
		{ID: 2, DisplayTitle: "Volumes", VolumeReadCount: 1, VolumeCount: 3},
		{ID: 3, DisplayTitle: "Done", ReadCount: 5, DiscoveredCount: 5},
		{ID: 4, DisplayTitle: "Untouched", DiscoveredCount: 9},
	}
	lastRead := map[int64]time.Time{1: now.Add(-time.Hour), 2: now, 3: now}
	row := continueReadingRow(titles, lastRead)
	if len(row.Cards) != 2 {
		t.Fatalf("cards = %+v", row.Cards)
	}
	if row.Cards[0].Title != "Volumes" || row.Cards[0].Href != "/reader/2?mode=volumes" {
		t.Fatalf("first card = %+v", row.Cards[0])
	}
	if row.Cards[1].Href != "/reader/1" || row.Cards[1].Percent != 30 {
		t.Fatalf("second card = %+v", row.Cards[1])
	}
}

func TestLatestArrivalsRowByAddDate(t *testing.T) {
	now := time.Now()
	titles := []library.Title{
		{ID: 1, DisplayTitle: "Older", CreatedAt: now.Add(-48 * time.Hour)},
		{ID: 2, DisplayTitle: "Newer", CreatedAt: now.Add(-1 * time.Hour)},
	}
	row := latestArrivalsRow("Latest arrivals", titles, nil)
	if len(row.Cards) != 2 {
		t.Fatalf("cards = %+v", row.Cards)
	}
	if row.Cards[0].Title != "Newer" || row.Cards[0].Href != "/library/2" {
		t.Fatalf("newest-added should sort first: %+v", row.Cards[0])
	}
	if row.Cards[0].Sub == "" || row.Cards[0].Sub == "Added" {
		t.Errorf("arrival sub should carry a relative add time, got %q", row.Cards[0].Sub)
	}
}

func TestLatestUpdatesRowByDownloadedChapter(t *testing.T) {
	now := time.Now()
	titles := []library.Title{
		{ID: 1, DisplayTitle: "Stale", CreatedAt: now}, // newest add, but oldest download
		{ID: 2, DisplayTitle: "Fresh", CreatedAt: now.Add(-72 * time.Hour)},
		{ID: 3, DisplayTitle: "NoDownloads", CreatedAt: now},
	}
	arrivals := map[int64]library.Arrival{
		1: {At: now.Add(-72 * time.Hour), Label: "Ch 5"},
		2: {At: now.Add(-time.Hour), Label: "Ch 40"},
	}
	row := latestUpdatesRow("Latest updates", titles, arrivals, nil)
	if len(row.Cards) != 2 { // title 3 has no downloads -> excluded
		t.Fatalf("cards = %+v", row.Cards)
	}
	if row.Cards[0].Title != "Fresh" || row.Cards[0].Sub != "Ch 40" {
		t.Fatalf("most-recently-downloaded should sort first: %+v", row.Cards[0])
	}
}
