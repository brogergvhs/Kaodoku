package chapters

import (
	"regexp"
	"strconv"
	"strings"
)

// DisplayParts is a scraped chapter title prepared for display next to the
// chapter's own number.
type DisplayParts struct {
	Name   string // title minus the redundant "Chapter <label>" prefix
	Volume string // leading volume marker, e.g. "Vol. 3", or ""
}

// The number must be followed by a separator, whitespace, or the end, so a
// split-chapter suffix ("Chapter 10a") is never torn apart mid-token.
// Separators include fullwidth ：．， from CJK-scraped titles.
var (
	reChapterPrefix = regexp.MustCompile(`(?i)^\s*ch(?:apter)?\.?\s*([0-9]+(?:\.[0-9]+)?)(?:\s*[-:.,–—：．，]+\s*|\s+|$)`)
	reVolumePrefix  = regexp.MustCompile(`(?i)^\s*vol(?:ume)?\.?\s*([0-9]+(?:\.[0-9]+)?)(?:\s*[-:.,–—：．，]+\s*|\s+|$)`)
)

// DisplayName strips the parts of a scraped title that would repeat the
// chapter number shown next to it, and pulls a leading volume marker out
// into its own string. Only display is affected: the stored title keeps the
// volume marker for the future volume-generation feature. A title that is
// nothing but "Chapter <label>" comes back empty — the number alone is the
// whole name.
func DisplayName(label, title string) DisplayParts {
	name := strings.TrimSpace(title)
	var volume string
	// Two passes: handles both "Chapter 1: Vol.1 ..." and "Vol.1 Chapter 1 ...".
	for range 2 {
		if m := reChapterPrefix.FindStringSubmatch(name); m != nil && sameNumber(m[1], label) {
			name = name[len(m[0]):]
		}
		if m := reVolumePrefix.FindStringSubmatch(name); m != nil && volume == "" {
			volume = "Vol. " + m[1]
			name = name[len(m[0]):]
		}
	}
	return DisplayParts{Name: strings.TrimSpace(name), Volume: volume}
}

// sameNumber compares "09" to "9" and "9.5" to "9.5" numerically, so the
// prefix is only dropped when it really repeats the chapter's own number.
func sameNumber(a, b string) bool {
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(strings.TrimSpace(b), 64)
	return errA == nil && errB == nil && fa == fb
}
